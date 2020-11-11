package main

import (
	"fmt"
	"github.com/pkg/errors"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/urfave/cli/v2"
)

const (
	// IMPORTANT should be synced with the runtime-spec dependency in go.mod
	// github.com/opencontainers/runtime-spec v1.0.2
	CURRENT_OCI_VERSION = "1.0.2"
	// Environment variables are populated by default from this environment file.
	// Existing environment variables are preserved.
	EnvFileDefault = "/etc/default/crio-lxc"
	// This environment variable can be used to overwrite the path in EnvFileDefault.
	EnvFileVar = "CRIO_LXC_DEFAULTS"
)

var version string
var clxc CrioLXC

func main() {
	app := cli.NewApp()
	app.Name = "crio-lxc"
	app.Usage = "crio-lxc is a CRI compliant runtime wrapper for lxc"
	app.Version = clxc.VersionString()
	app.Commands = []*cli.Command{
		&stateCmd,
		&createCmd,
		&startCmd,
		&killCmd,
		&deleteCmd,
		&execCmd,
	}

	app.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:        "log-level",
			Usage:       "set log level (trace|debug|info|warn|error)",
			EnvVars:     []string{"CRIO_LXC_LOG_LEVEL"},
			Value:       "warn",
			Destination: &clxc.LogLevelString,
		},
		&cli.StringFlag{
			Name:        "log-file",
			Usage:       "log file for LXC and crio-lxc (default is per container in lxc-path)",
			EnvVars:     []string{"CRIO_LXC_LOG_FILE", "LOG_FILE"},
			Value:       "/var/log/crio-lxc.log",
			Destination: &clxc.LogFilePath,
		},
		&cli.StringFlag{
			Name:        "backup-dir",
			Usage:       "directory for container runtime directory backups",
			EnvVars:     []string{"CRIO_LXC_BACKUP_DIR"},
			Value:       "/var/lib/crio-lxc/backup",
			Destination: &clxc.BackupDir,
		},
		&cli.BoolFlag{
			Name:        "backup-on-error",
			Usage:       "backup container runtime directory when cmd-start fails",
			EnvVars:     []string{"CRIO_LXC_BACKUP_ON_ERROR"},
			Value:       true,
			Destination: &clxc.BackupOnError,
		},
		&cli.BoolFlag{
			Name:        "backup",
			Usage:       "backup every container runtime before cmd-start is called",
			EnvVars:     []string{"CRIO_LXC_BACKUP"},
			Value:       false,
			Destination: &clxc.Backup,
		},
		&cli.StringFlag{
			Name:        "root",
			Aliases:     []string{"lxc-path"}, // 'root' is used by crio/conmon
			Usage:       "set the root path where container resources are created (logs, init and hook scripts). Must have access permissions",
			Value:       "/var/lib/lxc",
			Destination: &clxc.RuntimeRoot,
		},
		&cli.BoolFlag{
			Name:        "systemd-cgroup",
			Usage:       "enable systemd cgroup",
			Destination: &clxc.SystemdCgroup,
		},
		&cli.StringFlag{
			Name:        "monitor-cgroup",
			Usage:       "cgroup for LXC monitor processes",
			Destination: &clxc.MonitorCgroup,
			EnvVars:     []string{"CRIO_LXC_MONITOR_CGROUP"},
			Value:       "crio-lxc-monitor.slice",
		},
		&cli.StringFlag{
			Name:        "cmd-init",
			Usage:       "Absolute path to container init binary",
			EnvVars:     []string{"CRIO_LXC_CMD_INIT"},
			Value:       "/usr/local/bin/crio-lxc-init",
			Destination: &clxc.InitCommand,
		},
		&cli.StringFlag{
			Name:        "cmd-start",
			Usage:       "Name or path to container start binary",
			EnvVars:     []string{"CRIO_LXC_CMD_START"},
			Value:       "crio-lxc-start",
			Destination: &clxc.StartCommand,
		},
		&cli.StringFlag{
			Name:        "cmd-hook",
			Usage:       "Name or path to binary executed in lxc.hook.mount",
			EnvVars:     []string{"CRIO_LXC_CMD_HOOK"},
			Value:       "/usr/local/bin/crio-lxc-hook",
			Destination: &clxc.HookCommand,
		},
		&cli.BoolFlag{
			Name:        "seccomp",
			Usage:       "Generate and apply seccomp profile for lxc from container spec",
			Destination: &clxc.Seccomp,
			EnvVars:     []string{"CRIO_LXC_SECCOMP"},
			Value:       true,
		},
		&cli.BoolFlag{
			Name:        "capabilities",
			Usage:       "Keep capabilities defined in container spec",
			Destination: &clxc.Capabilities,
			EnvVars:     []string{"CRIO_LXC_CAPABILITIES"},
			Value:       true,
		},
		&cli.BoolFlag{
			Name:        "apparmor",
			Usage:       "Set apparmor profile defined in container spec",
			Destination: &clxc.Apparmor,
			EnvVars:     []string{"CRIO_LXC_APPARMOR"},
			Value:       true,
		},
		&cli.BoolFlag{
			Name:        "cgroup-devices",
			Usage:       "Allow only devices permitted by container spec",
			Destination: &clxc.CgroupDevices,
			EnvVars:     []string{"CRIO_LXC_CGROUP_DEVICES"},
			Value:       true,
		},
	}

	startTime := time.Now()

	app.Before = func(ctx *cli.Context) error {
		clxc.Command = ctx.Args().Get(0)
		return nil
	}

	setupCmd := func(ctx *cli.Context) error {
		containerID := ctx.Args().Get(0)
		if len(containerID) == 0 {
			return errors.New("missing container ID")
		}
		clxc.ContainerID = containerID
		clxc.Command = ctx.Command.Name

		if err := clxc.configureLogging(); err != nil {
			return err
		}

		for _, env := range os.Environ() {
			log.Trace().Str("env:", env).Msg("effective environment variable")
		}
		for _, appFlag := range app.Flags {
			name := appFlag.Names()[0]
			log.Trace().Str("name:", name).Str("value:", ctx.String(name)).Msg("effective cmdline flag")
		}

		log.Info().Strs("args", os.Args).Msg("run cmd")
		return nil
	}

	// Disable the default error messages for cmdline errors.
	// By default the app/cmd help is printed to stdout, which produces garbage in cri-o log output.
	// Instead the cmdline is reflected to identify cmdline interface errors
	errUsage := func(context *cli.Context, err error, isSubcommand bool) error {
		fmt.Fprintf(os.Stderr, "usage error %s: %s\n", err, os.Args)
		return err
	}
	app.OnUsageError = errUsage

	for _, cmd := range app.Commands {
		cmd.Before = setupCmd
		cmd.OnUsageError = errUsage
	}

	app.CommandNotFound = func(ctx *cli.Context, cmd string) {
		fmt.Fprintf(os.Stderr, "undefined subcommand %q cmdline%s\n", cmd, os.Args)
	}

	envFile := EnvFileDefault
	if s, isSet := os.LookupEnv(EnvFileVar); isSet {
		envFile = s
	}
	if err := loadEnvDefaults(envFile); err != nil {
		println(err.Error())
		os.Exit(1)
	}

	err := app.Run(os.Args)
	cmdDuration := time.Since(startTime)
	if err != nil {
		log.Error().Err(err).Dur("duration:", cmdDuration).Msg("cmd failed")
	} else {
		log.Info().Dur("duration:", cmdDuration).Msg("cmd done")
	}

	clxc.Release()
	if err != nil {
		// write diagnostics message to stderr for crio/kubelet
		println(err.Error())
		os.Exit(1)
	}
}

// TODO This should be added to the urfave/cli API - create a pull request
func loadEnvDefaults(envFile string) error {
	_, err := os.Stat(envFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return errors.Wrapf(err, "failed to stat %s", envFile)
	}
	data, err := ioutil.ReadFile(envFile)
	if err != nil {
		return errors.Wrap(err, "failed to load env file")
	}
	lines := strings.Split(string(data), "\n")
	for n, line := range lines {
		trimmed := strings.TrimSpace(line)
		//skip over comments and blank lines
		if len(trimmed) == 0 || trimmed[0] == '#' {
			continue
		}
		vals := strings.SplitN(trimmed, "=", 2)
		if len(vals) != 2 {
			return fmt.Errorf("Invalid environment variable at %s +%d", envFile, n)
		}
		key := strings.TrimSpace(vals[0])
		val := strings.Trim(strings.TrimSpace(vals[1]), `"'`)
		// existing environment variables have precedence
		if _, exist := os.LookupEnv(key); !exist {
			os.Setenv(key, val)
		}
	}
	return nil
}
