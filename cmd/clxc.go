package main

import (
	"fmt"
	"github.com/pkg/errors"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/lxc/crio-lxc/cmd/internal"
	"github.com/rs/zerolog"
	"gopkg.in/lxc/go-lxc.v2"
)

// time format used for logger
const timeFormatLXCMillis = "20060102150405.000"

var log zerolog.Logger

var errContainerNotExist = errors.New("container does not exist")
var errContainerExist = errors.New("container already exists")

type crioLXC struct {
	Container *lxc.Container

	Command string

	// [ global settings ]
	RuntimeRoot    string
	ContainerID    string
	LogFile        *os.File
	LogFilePath    string
	LogLevel       lxc.LogLevel
	LogLevelString string
	BackupDir      string
	Backup         bool
	BackupOnError  bool
	SystemdCgroup  bool
	MonitorCgroup  string
	StartCommand   string
	InitCommand    string
	HookCommand    string

	// feature gates
	Seccomp       bool
	Capabilities  bool
	Apparmor      bool
	CgroupDevices bool

	// create flags
	BundlePath    string
	SpecPath      string // BundlePath + "/config.json"
	PidFile       string
	ConsoleSocket string
	CreateTimeout time.Duration

	// start flags
	StartTimeout time.Duration
}

var version string

func versionString() string {
	return fmt.Sprintf("%s (%s) (lxc:%s)", version, runtime.Version(), lxc.Version())
}

// runtimePath builds an absolute filepath which is relative to the container runtime root.
func (c *crioLXC) runtimePath(subPath ...string) string {
	return filepath.Join(c.RuntimeRoot, c.ContainerID, filepath.Join(subPath...))
}

func (c *crioLXC) loadContainer() error {
	// Check for container existence by looking for config file.
	// Otherwise lxc.NewContainer will return an empty container
	// struct and we'll report wrong info
	_, err := os.Stat(c.runtimePath("config"))
	if os.IsNotExist(err) {
		return errContainerNotExist
	}
	if err != nil {
		return errors.Wrap(err, "failed to access config file")
	}

	container, err := lxc.NewContainer(c.ContainerID, c.RuntimeRoot)
	if err != nil {
		return errors.Wrap(err, "failed to load container")
	}
	if err := container.LoadConfigFile(c.runtimePath("config")); err != nil {
		return err
	}
	c.Container = container
	return nil
}

func (c *crioLXC) createContainer() error {
	_, err := os.Stat(c.runtimePath("config"))
	if !os.IsNotExist(err) {
		return errContainerExist
	}
	container, err := lxc.NewContainer(c.ContainerID, c.RuntimeRoot)
	if err != nil {
		return err
	}
	c.Container = container
	if err := os.MkdirAll(c.runtimePath(), 0770); err != nil {
		return errors.Wrap(err, "failed to create container dir")
	}
	return nil
}

// Release releases/closes allocated resources (lxc.Container, LogFile)
func (c crioLXC) release() error {
	if c.Container != nil {
		if err := c.Container.Release(); err != nil {
			log.Error().Err(err).Msg("failed to release container")
		}
	}
	if c.LogFile != nil {
		return c.LogFile.Close()
	}
	return nil
}

func supportsConfigItem(keys ...string) bool {
	for _, key := range keys {
		if !lxc.IsSupportedConfigItem(key) {
			log.Debug().Str("key:", key).Msg("unsupported lxc config item")
			return false
		}
	}
	return true
}

func (c *crioLXC) getConfigItem(key string) string {
	vals := c.Container.ConfigItem(key)
	if len(vals) > 0 {
		first := vals[0]
		// some lxc config values are set to '(null)' if unset
		// eg. lxc.cgroup.dir
		if first != "(null)" {
			return first
		}
	}
	return ""
}

func (c *crioLXC) setConfigItem(key, value string) error {
	err := c.Container.SetConfigItem(key, value)
	if err != nil {
		log.Error().Err(err).Str("key:", key).Str("value:", value).Msg("lxc config")
	} else {
		log.Debug().Str("key:", key).Str("value:", value).Msg("lxc config")
	}
	return errors.Wrap(err, "failed to set lxc config item '%s=%s'")
}

func (c *crioLXC) configureLogging() error {
	logDir := filepath.Dir(c.LogFilePath)
	err := os.MkdirAll(logDir, 0750)
	if err != nil {
		return errors.Wrapf(err, "failed to create log file directory %s", logDir)
	}

	f, err := os.OpenFile(c.LogFilePath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
	if err != nil {
		return errors.Wrapf(err, "failed to open log file %s", c.LogFilePath)
	}
	c.LogFile = f

	zerolog.TimestampFieldName = "t"
	zerolog.LevelFieldName = "p"
	zerolog.MessageFieldName = "m"
	zerolog.TimeFieldFormat = timeFormatLXCMillis

	// NOTE It's not possible change the possition of the timestamp.
	// The ttimestamp is appended to the to the log output because it is dynamically rendered
	// see https://github.com/rs/zerolog/issues/109
	log = zerolog.New(f).With().Timestamp().Str("cmd:", c.Command).Str("cid:", c.ContainerID).Logger()

	level, err := parseLogLevel(c.LogLevelString)
	if err != nil {
		log.Error().Err(err).Stringer("loglevel:", level).Msg("using fallback log-level")
	}
	c.LogLevel = level

	switch level {
	case lxc.TRACE:
		zerolog.SetGlobalLevel(zerolog.TraceLevel)
	case lxc.DEBUG:
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	case lxc.INFO:
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	case lxc.WARN:
		zerolog.SetGlobalLevel(zerolog.WarnLevel)
	case lxc.ERROR:
		zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	}
	return nil
}

func parseLogLevel(s string) (lxc.LogLevel, error) {
	switch strings.ToLower(s) {
	case "trace":
		return lxc.TRACE, nil
	case "debug":
		return lxc.DEBUG, nil
	case "info":
		return lxc.INFO, nil
	case "warn":
		return lxc.WARN, nil
	case "error":
		return lxc.ERROR, nil
	default:
		return lxc.ERROR, fmt.Errorf("Invalid log-level %s", s)
	}
}

// BackupRuntimeResources creates a backup of the container runtime resources.
// It returns the path to the backup directory.
//
// The following resources are backed up:
// - all resources created by crio-lxc (lxc config, init script, device creation script ...)
// - lxc logfiles (if logging is setup per container)
// - the runtime spec
func (c *crioLXC) backupRuntimeResources() (backupDir string, err error) {
	backupDir = filepath.Join(c.BackupDir, c.ContainerID)
	err = os.MkdirAll(c.BackupDir, 0700)
	if err != nil {
		return "", errors.Wrap(err, "failed to create backup dir")
	}
	err = runCommand("cp", "-r", "-p", clxc.runtimePath(), backupDir)
	if err != nil {
		return backupDir, errors.Wrap(err, "failed to copy lxc runtime directory")
	}
	// remove syncfifo because it is not of any use and blocks 'grep' within the backup directory.
	os.Remove(filepath.Join(backupDir, internal.SyncFifoPath))
	err = runCommand("cp", clxc.SpecPath, backupDir)
	if err != nil {
		return backupDir, errors.Wrap(err, "failed to copy runtime spec to backup dir")
	}
	return backupDir, nil
}

// TODO avoid shellout
func runCommand(args ...string) error {
	cmd := exec.Command(args[0], args[1:]...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return errors.Errorf("%s: %s: %s", strings.Join(args, " "), err, string(output))
	}
	return nil
}

// runtime states https://github.com/opencontainers/runtime-spec/blob/v1.0.2/runtime.md
const (
	// the container is being created (step 2 in the lifecycle)
	stateCreating = "creating"
	// the runtime has finished the create operation (after step 2 in the lifecycle),
	// and the container process has neither exited nor executed the user-specified program
	stateCreated = "created"
	// the container process has executed the user-specified program
	// but has not exited (after step 5 in the lifecycle)
	stateRunning = "running"
	// the container process has exited (step 7 in the lifecycle)
	stateStopped = "stopped"

	// crio-lxc-init is started but blocking at the syncfifo
	envStateCreated = "CRIO_LXC_STATE=" + stateCreated
)

func (c *crioLXC) getContainerState() (int, string, error) {
	switch state := c.Container.State(); state {
	case lxc.STARTING:
		return 0, stateCreating, nil
	case lxc.STOPPED:
		return 0, stateStopped, nil
	default:
		return c.getContainerInitState()
	}
}

// getContainerInitState returns the runtime state of the container.
// It is used to determine whether the container state is 'created' or 'running'.
// The init process environment contains #envStateCreated if the the container
// is created, but not yet running/started.
// This requires the proc filesystem to be mounted on the host.
func (c *crioLXC) getContainerInitState() (int, string, error) {
	pid, proc := c.safeGetInitPid()
	if proc != nil {
		defer proc.Close()
	}
	if pid <= 0 {
		return 0, stateStopped, nil
	}

	envFile := fmt.Sprintf("/proc/%d/environ", pid)
	data, err := ioutil.ReadFile(envFile)
	if err != nil {
		// This is fatal. It should not happen because a filehandle to /proc/%d is open.
		return 0, stateStopped, errors.Wrapf(err, "failed to read init process environment %s", envFile)
	}

	environ := strings.Split(string(data), "\000")
	for _, env := range environ {
		if env == envStateCreated {
			return pid, stateCreated, nil
		}
	}
	return pid, stateRunning, nil
}

func (c *crioLXC) safeGetInitPid() (pid int, proc *os.File) {
	pid = c.Container.InitPid()
	if pid <= 0 {
		// Errors returned from safeGetInitPid indicate that the init process has died.
		return 0, nil
	}
	// Open the proc directory of the init process to avoid that
	// it's PID is recycled before it receives the signal.
	proc, err := os.Open(fmt.Sprintf("/proc/%d", pid))

	// double check that the init process still exists, and the proc
	// directory actually belongs to the init process.
	pid2 := c.Container.InitPid()
	if pid2 != pid {
		if proc != nil {
			proc.Close()
		}
		// init process has died which should only happen if /proc/%d was not opened
		return 0, nil
	}

	// The init PID still exists, but /proc/{pid} can not be opened.
	// The only reason maybe that the proc filesystem is not mounted.
	// It's unlikely a permissions problem because crio runs as privileged process.
	// This leads to race conditions and should appear in the logs.
	if proc == nil {
		log.Error().Err(err).Int("pid:", pid).Msg("failed to open /proc directory for init PID - procfs mounted?")
	}

	return pid, proc
}

func (c *crioLXC) waitContainerCreated(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		log.Trace().Msg("poll for container init state")
		pid, state, err := c.getContainerInitState()
		if err != nil {
			return errors.Wrap(err, "failed to wait for container container creation")
		}

		if pid > 0 && state == stateCreated {
			return nil
		}
		time.Sleep(time.Millisecond * 50)
	}
	return fmt.Errorf("timeout (%s) waiting for container creation", timeout)
}
