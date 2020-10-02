package main

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	"github.com/containerd/containerd/platforms"
	"github.com/containerd/cri/pkg/api/criextension"
	"github.com/containerd/cri/pkg/client"
	errorpkg "github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"google.golang.org/grpc"
	"gopkg.in/yaml.v2"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

const defaultConfigName = "crictl.yaml"

var globalConfig *Config

type Config struct {
	RuntimeEndpoint string `yaml:"runtime-endpoint"`
	Timeout         int    `yaml:"timeout"`
	Debug           bool   `yaml:"debug"`
}

// readConfig reads from a file with the given name and returns a config or
// an error if the file was unable to be parsed.
func readConfig(filepath string) (*Config, error) {
	data, err := ioutil.ReadFile(filepath)
	if err != nil {
		return nil, err
	}
	config := &Config{}
	err = yaml.Unmarshal(data, config)
	if err != nil {
		return nil, err
	}
	return config, err
}

func getDefaultConfigPath() string {
	dir := filepath.Base(os.Args[0])
	return filepath.Join(dir, defaultConfigName)
}

func getRuntimeClientConnection() (*grpc.ClientConn, error) {
	// dial the connection
	if globalConfig == nil {
		return nil, errors.New("criextension's config is not set")
	}
	if globalConfig.RuntimeEndpoint == "" {
		return nil, errors.New("criextension's config does not contain a runtime endpoint to use for this command")
	}
	timeout := time.Duration(globalConfig.Timeout) * time.Second
	addr, dialer, err := client.GetAddressAndDialer(globalConfig.RuntimeEndpoint)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.Dial(addr, grpc.WithInsecure(), grpc.WithBlock(), grpc.WithTimeout(timeout), grpc.WithDialer(dialer))
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func getRuntimeClient() (criextension.CRIExtensionRuntimeServiceClient, *grpc.ClientConn, error) {
	conn, err := getRuntimeClientConnection()
	if err != nil {
		return nil, nil, errorpkg.Wrap(err, "failed to connect to runtime client")
	}
	client := criextension.NewCRIExtensionRuntimeServiceClient(conn)
	return client, conn, nil
}

func main() {
	app := cli.NewApp()
	app.Name = "criextension"
	app.Usage = "tool for running the criextension service"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "config",
			Value: getDefaultConfigPath(),
			Usage: "Location of the client config file. If not specified, searches the executable's directory.",
		},
	}
	app.Commands = []cli.Command{
		updateContainerCommand,
	}
	app.Before = func(cli *cli.Context) (err error) {
		// read from file, have default location
		config, err := readConfig(cli.String("config"))
		if err != nil {
			logrus.Fatal(err)
			return err
		}
		globalConfig = config
		return nil
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

var updateContainerCommand = cli.Command{
	Name:      "updateContainer",
	Usage:     "updates container",
	ArgsUsage: "CONTAINER-ID [flags...]",
	Flags: []cli.Flag{
		cli.Int64Flag{
			Name:  "cpu-share",
			Usage: "CPU shares (relative weight vs. other containers)",
		},
		cli.Int64Flag{
			Name:  "memory",
			Usage: "Memory limit (in bytes)",
		},
		cli.Int64Flag{
			Name:  "cpu-period",
			Usage: "CPU CFS period to be used for hardcapping (in usecs). 0 to use system default",
		},
		cli.Int64Flag{
			Name:  "cpu-quota",
			Usage: "CPU CFS hardcap limit (in usecs). Allowed cpu time in a given period",
		},
		cli.StringFlag{
			Name:  "cpuset-cpus",
			Usage: "CPU(s) to use",
		},
		cli.StringFlag{
			Name:  "cpuset-mems",
			Usage: "Memory node(s) to use",
		},
		cli.Int64Flag{
			Name:  "cpu-count",
			Usage: "Number of CPU(s) available to the container.",
		},
		cli.Int64Flag{
			Name:  "cpu-max",
			Usage: "Specifies the portion of processor cycles that this container can use as a percentage times 100.",
		},
		cli.StringFlag{
			Name:  "platform",
			Usage: "For specifying the container platform, default to running platform.",
		},
	},
	Action: func(cli *cli.Context) error {
		if globalConfig == nil {
			return errors.New("criextension's config is not set")
		}
		client, conn, err := getRuntimeClient()
		if err != nil {
			return err
		}
		defer func() {
			if conn != nil {
				conn.Close()
			}
		}()
		platform := platforms.DefaultSpec().OS
		if cli.String("platform") != "" {
			platform = cli.String("platform")
		}

		/*cpuGroupResources := &criextension.CPUGroupResources{
			Id:                cli.String("cpu-group-id"),
			Cap:               cli.Int64("cpu-cap"),
			SchedulerPriority: cli.Int64("scheduler-priority"),
		}*/

		parsedResources := &criextension.COWContainerResourcesV2{}

		if platform == "windows" {
			parsedResources.StdWindowsResources = &runtime.WindowsContainerResources{
				CpuShares:          cli.Int64("cpu-share"),
				CpuCount:           cli.Int64("cpu-count"),
				CpuMaximum:         cli.Int64("cpu-max"),
				MemoryLimitInBytes: cli.Int64("memory"),
			}
		} else if platform == "linux" {
			parsedResources.StdLinuxResources = &runtime.LinuxContainerResources{
				CpuPeriod:          cli.Int64("cpu-period"),
				CpuQuota:           cli.Int64("cpu-quota"),
				CpuShares:          cli.Int64("cpu-share"),
				CpusetCpus:         cli.String("cpuset-cpus"),
				CpusetMems:         cli.String("cpuset-mems"),
				MemoryLimitInBytes: cli.Int64("memory"),
			}
		} else {
			return fmt.Errorf("platform %s not supported for updating container resources", platform)
		}

		cid := cli.Args().First()
		req := &criextension.UpdateContainerResourcesV2Request{
			ContainerId: cid,
			Resources:   parsedResources,
		}
		if _, err := client.UpdateContainerResourcesV2(context.Background(), req); err != nil {
			return errorpkg.Wrapf(err, "updating container resources for %s", cid)
		}
		return nil
	},
}
