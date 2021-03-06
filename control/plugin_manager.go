/*
http://www.apache.org/licenses/LICENSE-2.0.txt


Copyright 2015 Intel Corporation

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package control PluginManger manages loading, unloading, and swapping
// of plugins
package control

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/appc/spec/schema"

	"github.com/intelsdi-x/gomit"
	"github.com/intelsdi-x/snap/control/plugin"
	"github.com/intelsdi-x/snap/control/plugin/client"
	"github.com/intelsdi-x/snap/control/plugin/cpolicy"
	"github.com/intelsdi-x/snap/core"
	"github.com/intelsdi-x/snap/core/cdata"
	"github.com/intelsdi-x/snap/core/serror"
)

const (
	// DetectedState is the detected state of a plugin
	DetectedState pluginState = "detected"
	// LoadingState is the loading state of a plugin
	LoadingState pluginState = "loading"
	// LoadedState is the loaded state of a plugin
	LoadedState pluginState = "loaded"
	// UnloadedState is the unloaded state of a plugin
	UnloadedState pluginState = "unloaded"
)

var (
	// ErrPluginNotFound - error message when a plugin is not found
	ErrPluginNotFound = errors.New("plugin not found")
	// ErrPluginAlreadyLoaded - error message when a plugin is already loaded
	ErrPluginAlreadyLoaded = errors.New("plugin is already loaded")
	// ErrPluginNotInLoadedState - error message when a plugin must ne in a loaded state
	ErrPluginNotInLoadedState = errors.New("Plugin must be in a LoadedState")

	pmLogger = log.WithField("_module", "control-plugin-mgr")
)

type pluginState string

type loadedPlugins struct {
	*sync.RWMutex

	table map[string]*loadedPlugin
}

func newLoadedPlugins() *loadedPlugins {
	return &loadedPlugins{
		RWMutex: &sync.RWMutex{},
		table:   make(map[string]*loadedPlugin),
	}
}

// add adds a loadedPlugin pointer to the table
func (l *loadedPlugins) add(lp *loadedPlugin) serror.SnapError {
	l.Lock()
	defer l.Unlock()

	if _, exists := l.table[lp.Key()]; exists {
		return serror.New(ErrPluginAlreadyLoaded, map[string]interface{}{
			"plugin-name":    lp.Meta.Name,
			"plugin-version": lp.Meta.Version,
			"plugin-type":    lp.Type.String(),
		})
	}
	l.table[lp.Key()] = lp
	return nil
}

// get retrieves a plugin from the table
func (l *loadedPlugins) get(key string) (*loadedPlugin, error) {
	l.RLock()
	defer l.RUnlock()

	lp, ok := l.table[key]
	if !ok {
		tnv := strings.Split(key, core.Separator)
		if len(tnv) != 3 {
			return nil, ErrBadKey
		}

		v, err := strconv.Atoi(tnv[2])
		if err != nil {
			return nil, ErrBadKey
		}
		if v < 1 {
			pmLogger.Info("finding latest plugin")
			return l.findLatest(tnv[0], tnv[1])
		}
		return nil, ErrPluginNotFound
	}
	return lp, nil
}

func (l *loadedPlugins) remove(key string) {
	l.Lock()
	delete(l.table, key)
	l.Unlock()
}

func (l *loadedPlugins) findLatest(typeName, name string) (*loadedPlugin, error) {
	l.RLock()
	defer l.RUnlock()

	// quick check to see if there exists a plugin with the name/version we're looking for.
	// if not we just return ErrNotFound before we check versions.
	var latest *loadedPlugin
	for _, lp := range l.table {
		if lp.TypeName() == typeName && lp.Name() == name {
			latest = lp
			break
		}
	}

	if latest != nil {
		for _, lp := range l.table {
			if lp.TypeName() == typeName && lp.Name() == name && lp.Version() > latest.Version() {
				latest = lp
			}
		}
		return latest, nil
	}
	return nil, ErrPluginNotFound
}

// the struct representing a plugin that is loaded into snap
type pluginDetails struct {
	CheckSum  [sha256.Size]byte
	Exec      []string
	ExecPath  string
	IsPackage bool
	Manifest  *schema.ImageManifest
	Path      string
	Signed    bool
	Signature []byte
}

type loadedPlugin struct {
	Meta         plugin.PluginMeta
	Details      *pluginDetails
	Type         plugin.PluginType
	State        pluginState
	Token        string
	LoadedTime   time.Time
	ConfigPolicy *cpolicy.ConfigPolicy
}

// Name returns plugin name
// implements the CatalogedPlugin interface
func (lp *loadedPlugin) Name() string {
	return lp.Meta.Name
}

// PluginPath returns the plugin path
func (lp *loadedPlugin) PluginPath() string {
	return lp.Details.Path
}

// Key returns plugin type, name and version
func (lp *loadedPlugin) Key() string {
	return fmt.Sprintf("%s"+core.Separator+"%s"+core.Separator+"%d", lp.TypeName(), lp.Name(), lp.Version())
}

// Version returns plugin version
// implements the CatalogedPlugin interface
func (lp *loadedPlugin) Version() int {
	return lp.Meta.Version
}

// TypeName returns plugin type as a string
// implements the CatalogedPlugin interface
func (lp *loadedPlugin) TypeName() string {
	return lp.Type.String()
}

// Status returns current plugin state
// implements the CatalogedPlugin interface
func (lp *loadedPlugin) Status() string {
	return string(lp.State)
}

// IsSigned returns plugin signing as a bool
// implements the CatalogedPlugin interface
func (lp *loadedPlugin) IsSigned() bool {
	return lp.Details.Signed
}

// LoadedTimestamp returns a unix timestamp of the LoadTime of a plugin
// implements the CatalogedPlugin interface
func (lp *loadedPlugin) LoadedTimestamp() *time.Time {
	return &lp.LoadedTime
}

func (lp *loadedPlugin) Policy() *cpolicy.ConfigPolicy {
	return lp.ConfigPolicy
}

// the struct representing the object responsible for
// loading and unloading plugins
type pluginManager struct {
	pluginLoadTimeout int
	metricCatalog     catalogsMetrics
	loadedPlugins     *loadedPlugins
	logPath           string
	pluginConfig      *pluginConfig
	pluginTags        map[string]map[string]string
	pprof             bool
	tempDirPath       string
}

func newPluginManager(opts ...pluginManagerOpt) *pluginManager {
	logPath := "/tmp"
	if runtime.GOOS == "windows" {
		logPath = `c:\temp`
	}
	p := &pluginManager{
		pluginLoadTimeout: defaultPluginLoadTimeout,
		loadedPlugins:     newLoadedPlugins(),
		logPath:           logPath,
		pluginConfig:      newPluginConfig(),
		pluginTags:        newPluginTags(),
	}

	for _, opt := range opts {
		opt(p)
	}

	return p
}

type pluginManagerOpt func(*pluginManager)

// OptSetPprof sets the pprof flag on the plugin manager
func OptSetTempDirPath(path string) pluginManagerOpt {
	return func(p *pluginManager) {
		p.tempDirPath = path
	}
}

// OptSetPprof sets the pprof flag on the plugin manager
func OptSetPprof(pprof bool) pluginManagerOpt {
	return func(p *pluginManager) {
		p.pprof = pprof
	}
}

// OptSetPluginConfig sets the config on the plugin manager
func OptSetPluginConfig(cf *pluginConfig) pluginManagerOpt {
	return func(p *pluginManager) {
		p.pluginConfig = cf
	}
}

// OptSetPluginTags sets the tags on the plugin manager
func OptSetPluginTags(tags map[string]map[string]string) pluginManagerOpt {
	return func(p *pluginManager) {
		p.pluginTags = tags
	}
}

// SetPluginLoadTimeout sets plugin load timeout
func (p *pluginManager) SetPluginLoadTimeout(to int) {
	p.pluginLoadTimeout = to
}

// SetPluginConfig sets plugin config
func (p *pluginManager) SetPluginConfig(cf *pluginConfig) {
	p.pluginConfig = cf
}

// GetPluginConfig gets the plugin config
func (p *pluginManager) GetPluginConfig() *pluginConfig {
	return p.pluginConfig
}

// SetPluginTags sets plugin tags
func (p *pluginManager) SetPluginTags(tags map[string]map[string]string) {
	p.pluginTags = tags
}

// SetMetricCatalog sets metric catalog
func (p *pluginManager) SetMetricCatalog(mc catalogsMetrics) {
	p.metricCatalog = mc
}

// LoadPlugin is the method for loading a plugin and
// saving plugin into the LoadedPlugins array
func (p *pluginManager) LoadPlugin(details *pluginDetails, emitter gomit.Emitter) (*loadedPlugin, serror.SnapError) {
	lPlugin := new(loadedPlugin)
	lPlugin.Details = details
	lPlugin.State = DetectedState

	pmLogger.WithFields(log.Fields{
		"_block": "load-plugin",
		"path":   filepath.Base(lPlugin.Details.Exec[0]),
	}).Info("plugin load called")

	// We will create commands by appending the ExecPath to the actual command.
	// The ExecPath is a temporary location where the plugin/package will be
	// run from.
	commands := make([]string, len(lPlugin.Details.Exec))
	for i, e := range lPlugin.Details.Exec {
		commands[i] = filepath.Join(lPlugin.Details.ExecPath, e)
	}

	ePlugin, err := plugin.NewExecutablePlugin(
		p.GenerateArgs(int(log.GetLevel())),
		commands...)
	if err != nil {
		pmLogger.WithFields(log.Fields{
			"_block": "load-plugin",
			"error":  err.Error(),
		}).Error("load plugin error while creating executable plugin")
		return nil, serror.New(err)
	}

	pmLogger.WithFields(log.Fields{
		"_block": "load-plugin",
		"path":   lPlugin.Details.Exec,
	}).Debug(fmt.Sprintf("plugin load timeout set to %ds", p.pluginLoadTimeout))
	resp, err := ePlugin.Run(time.Second * time.Duration(p.pluginLoadTimeout))
	if err != nil {
		pmLogger.WithFields(log.Fields{
			"_block": "load-plugin",
			"error":  err.Error(),
		}).Error("load plugin error when starting plugin")
		return nil, serror.New(err)
	}

	ePlugin.SetName(resp.Meta.Name)

	key := fmt.Sprintf("%s"+core.Separator+"%s"+core.Separator+"%d", resp.Meta.Type.String(), resp.Meta.Name, resp.Meta.Version)
	if _, exists := p.loadedPlugins.table[key]; exists {
		return nil, serror.New(ErrPluginAlreadyLoaded, map[string]interface{}{
			"plugin-name":    resp.Meta.Name,
			"plugin-version": resp.Meta.Version,
			"plugin-type":    resp.Type.String(),
		})
	}

	ap, err := newAvailablePlugin(resp, emitter, ePlugin)
	if err != nil {
		pmLogger.WithFields(log.Fields{
			"_block": "load-plugin",
			"error":  err.Error(),
		}).Error("load plugin error while creating available plugin")
		return nil, serror.New(err)
	}

	if resp.Meta.Unsecure {
		err = ap.client.Ping()
	} else {
		err = ap.client.SetKey()
	}

	if err != nil {
		pmLogger.WithFields(log.Fields{
			"_block": "load-plugin",
			"error":  err.Error(),
		}).Error("load plugin error while pinging the plugin")
		return nil, serror.New(err)
	}

	// Get the ConfigPolicy and add it to the loaded plugin
	c, ok := ap.client.(plugin.Plugin)
	if !ok {
		return nil, serror.New(errors.New("missing GetConfigPolicy function"))
	}
	cp, err := c.GetConfigPolicy()
	if err != nil {
		pmLogger.WithFields(log.Fields{
			"_block":         "load-plugin",
			"plugin-type":    "collector",
			"error":          err.Error(),
			"plugin-name":    ap.Name(),
			"plugin-version": ap.Version(),
			"plugin-id":      ap.ID(),
		}).Error("error in getting config policy")
		return nil, serror.New(err)
	}

	lPlugin.ConfigPolicy = cp
	lPlugin.Meta = resp.Meta
	lPlugin.Type = resp.Type
	lPlugin.Token = resp.Token
	lPlugin.LoadedTime = time.Now()
	lPlugin.State = LoadedState

	if resp.Type == plugin.CollectorPluginType {
		cfgNode := p.pluginConfig.getPluginConfigDataNode(core.PluginType(resp.Type), resp.Meta.Name, resp.Meta.Version)

		if lPlugin.ConfigPolicy != nil {
			// Get plugin config defaults
			defaults := cdata.NewNode()
			for _, cpolicy := range lPlugin.ConfigPolicy.GetAll() {
				_, errs := cpolicy.AddDefaults(defaults.Table())
				if len(errs.Errors()) > 0 {
					for _, err := range errs.Errors() {
						pmLogger.WithFields(log.Fields{
							"_block":         "load-plugin",
							"plugin-type":    "collector",
							"plugin-name":    ap.Name(),
							"plugin-version": ap.Version(),
							"plugin-id":      ap.ID(),
						}).Error(err.Error())
					}
					return nil, serror.New(errors.New("error getting default config"))
				}
			}

			// Update config policy with defaults
			cfgNode = cfgNode.ReverseMerge(defaults)
			cp, err = c.GetConfigPolicy()
			if err != nil {
				pmLogger.WithFields(log.Fields{
					"_block":         "load-plugin",
					"plugin-type":    "collector",
					"error":          err.Error(),
					"plugin-name":    ap.Name(),
					"plugin-version": ap.Version(),
					"plugin-id":      ap.ID(),
				}).Error("error in getting config policy")
				return nil, serror.New(err)
			}
			lPlugin.ConfigPolicy = cp
		}

		colClient := ap.client.(client.PluginCollectorClient)

		cfg := plugin.ConfigType{
			ConfigDataNode: cfgNode,
		}

		metricTypes, err := colClient.GetMetricTypes(cfg)
		if err != nil {
			pmLogger.WithFields(log.Fields{
				"_block":      "load-plugin",
				"plugin-type": "collector",
				"error":       err.Error(),
			}).Error("error in getting metric types")
			return nil, serror.New(err)
		}

		// Add metric types to metric catalog
		for _, nmt := range metricTypes {
			// If the version is 0 default it to the plugin version
			// This honors the plugins explicit version but falls back
			// to the plugin version as default
			if nmt.Version() < 1 {
				// Since we have to override version we convert to a internal struct
				nmt = &metricType{
					namespace:          nmt.Namespace(),
					version:            resp.Meta.Version,
					lastAdvertisedTime: nmt.LastAdvertisedTime(),
					config:             nmt.Config(),
					data:               nmt.Data(),
					tags:               nmt.Tags(),
					description:        nmt.Description(),
					unit:               nmt.Unit(),
				}
			}
			// We quit and throw an error on bad metric versions (<1)
			// the is a safety catch otherwise the catalog will be corrupted
			if nmt.Version() < 1 {
				err := errors.New("Bad metric version from plugin")
				pmLogger.WithFields(log.Fields{
					"_block":           "load-plugin",
					"plugin-name":      resp.Meta.Name,
					"plugin-version":   resp.Meta.Version,
					"plugin-type":      resp.Meta.Type.String(),
					"plugin-path":      filepath.Base(lPlugin.Details.ExecPath),
					"metric-namespace": nmt.Namespace(),
					"metric-version":   nmt.Version(),
					"error":            err.Error(),
				}).Error("received metric with bad version")
				return nil, serror.New(err)
			}

			//Add standard tags
			nmt = p.AddStandardAndWorkflowTags(nmt, nil)

			if err := p.metricCatalog.AddLoadedMetricType(lPlugin, nmt); err != nil {
				pmLogger.WithFields(log.Fields{
					"_block":           "load-plugin",
					"plugin-name":      resp.Meta.Name,
					"plugin-version":   resp.Meta.Version,
					"plugin-type":      resp.Meta.Type.String(),
					"plugin-path":      filepath.Base(lPlugin.Details.ExecPath),
					"metric-namespace": nmt.Namespace(),
					"metric-version":   nmt.Version(),
					"error":            err.Error(),
				}).Error("error adding loaded metric type")
				return nil, serror.New(err)
			}
		}
	}

	// Added so clients can adequately clean up connections
	ap.client.Kill("Retrieved necessary plugin info")
	err = ePlugin.Kill()
	if err != nil {
		pmLogger.WithFields(log.Fields{
			"_block": "load-plugin",
			"error":  err.Error(),
		}).Error("load plugin error while killing plugin executable plugin")
		return nil, serror.New(err)
	}

	if resp.State != plugin.PluginSuccess {
		e := fmt.Errorf("Plugin loading did not succeed: %s\n", resp.ErrorMessage)
		pmLogger.WithFields(log.Fields{
			"_block":          "load-plugin",
			"error":           e,
			"plugin response": resp.ErrorMessage,
		}).Error("load plugin error")
		return nil, serror.New(e)
	}

	aErr := p.loadedPlugins.add(lPlugin)
	if aErr != nil {
		pmLogger.WithFields(log.Fields{
			"_block": "load-plugin",
			"error":  aErr,
		}).Error("load plugin error while adding loaded plugin to load plugins collection")
		return nil, aErr
	}

	return lPlugin, nil
}

// UnloadPlugin unloads a plugin from the LoadedPlugins table
func (p *pluginManager) UnloadPlugin(pl core.Plugin) (*loadedPlugin, serror.SnapError) {
	plugin, err := p.loadedPlugins.get(fmt.Sprintf("%s"+core.Separator+"%s"+core.Separator+"%d", pl.TypeName(), pl.Name(), pl.Version()))
	if err != nil {
		se := serror.New(ErrPluginNotFound, map[string]interface{}{
			"plugin-name":    pl.Name(),
			"plugin-version": pl.Version(),
			"plugin-type":    pl.TypeName(),
		})
		return nil, se
	}
	pmLogger.WithFields(log.Fields{
		"_block": "unload-plugin",
		"path":   plugin.Details.Exec,
	}).Info("plugin unload called")

	if plugin.State != LoadedState {
		se := serror.New(ErrPluginNotInLoadedState, map[string]interface{}{
			"plugin-name":    plugin.Name(),
			"plugin-version": plugin.Version(),
			"plugin-type":    pl.TypeName(),
		})
		return nil, se
	}

	pmLogger.WithFields(log.Fields{
		"plugin-type":    plugin.TypeName(),
		"plugin-name":    plugin.Name(),
		"plugin-version": plugin.Version(),
		"plugin-path":    plugin.Details.Path,
	}).Debugf("Removing plugin")
	if strings.Contains(plugin.Details.Path, p.tempDirPath) {
		if err := os.RemoveAll(filepath.Dir(plugin.Details.Path)); err != nil {
			pmLogger.WithFields(log.Fields{
				"plugin-type":    plugin.TypeName(),
				"plugin-name":    plugin.Name(),
				"plugin-version": plugin.Version(),
				"plugin-path":    plugin.Details.Path,
			}).Error(err)
			se := serror.New(err)
			se.SetFields(map[string]interface{}{
				"plugin-type":    plugin.TypeName(),
				"plugin-name":    plugin.Name(),
				"plugin-version": plugin.Version(),
				"plugin-path":    plugin.Details.Path,
			})
			return nil, se
		}
	} else {
		pmLogger.WithFields(log.Fields{
			"plugin-type":    plugin.TypeName(),
			"plugin-name":    plugin.Name(),
			"plugin-version": plugin.Version(),
			"plugin-path":    plugin.Details.Path,
		}).Debug("Nothing to delete as temp path is empty")
	}

	p.loadedPlugins.remove(plugin.Key())

	// Remove any metrics from the catalog if this was a collector
	if plugin.TypeName() == "collector" {
		p.metricCatalog.RmUnloadedPluginMetrics(plugin)
	}

	return plugin, nil
}

// GenerateArgs generates the cli args to send when stating a plugin
func (p *pluginManager) GenerateArgs(logLevel int) plugin.Arg {
	return plugin.NewArg(logLevel, p.pprof)
}

func (p *pluginManager) teardown() {
	for _, lp := range p.loadedPlugins.table {
		_, err := p.UnloadPlugin(lp)
		if err != nil {
			runnerLog.WithFields(log.Fields{
				"plugin-type":    lp.TypeName(),
				"plugin-name":    lp.Name(),
				"plugin-version": lp.Version(),
				"plugin-path":    lp.Details.Path,
			}).Warn("error removing plugin in teardown:", err)
		}
	}
}

func (p *pluginManager) get(key string) (*loadedPlugin, error) {
	return p.loadedPlugins.get(key)
}

func (p *pluginManager) all() map[string]*loadedPlugin {
	p.loadedPlugins.RLock()
	defer p.loadedPlugins.RUnlock()
	return p.loadedPlugins.table
}

func hasPrefix(ns1 []string, ns2 []string) bool {
	for i := range ns2 {
		if i > len(ns1)-1 || ns1[i] != ns2[i] {
			return false
		}
	}
	return true
}

func split(ns string) []string {
	// the first character is the separator
	if len(ns) <= 1 {
		return nil
	}
	sep := string(ns[0])
	ns = strings.TrimSuffix(ns, sep)
	ns = strings.TrimPrefix(ns, sep)

	return strings.Split(ns, sep)
}

func (p *pluginManager) AddStandardAndWorkflowTags(m core.Metric, allTags map[string]map[string]string) core.Metric {
	hostname := hostnameReader.Hostname()

	tags := m.Tags()
	if tags == nil {
		tags = map[string]string{}
	}
	// apply standard tag
	tags[core.STD_TAG_PLUGIN_RUNNING_ON] = hostname

	// apply tags from global tags
	for ns, nsTags := range p.pluginTags {
		if hasPrefix(m.Namespace().Strings(), split(ns)) {
			for k, v := range nsTags {
				tags[k] = v
			}
		}
	}
	// apply tags from workflow
	for ns, nsTags := range allTags {
		if hasPrefix(m.Namespace().Strings(), split(ns)) {
			for k, v := range nsTags {
				tags[k] = v
			}
		}
	}

	metric := plugin.MetricType{
		Namespace_:          m.Namespace(),
		Version_:            m.Version(),
		LastAdvertisedTime_: m.LastAdvertisedTime(),
		Config_:             m.Config(),
		Data_:               m.Data(),
		Tags_:               tags,
		Description_:        m.Description(),
		Unit_:               m.Unit(),
		Timestamp_:          m.Timestamp(),
	}
	return metric
}
