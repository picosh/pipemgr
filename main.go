package main

import (
	"bufio"
	"context"
	"flag"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"time"

	ct "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/picosh/utils/pipe"
)

func createDockerClient() *client.Client {
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		slog.Error(
			"unable to create docker client",
			slog.Any("error", err),
		)
		panic(err)
	}

	return dockerClient
}

func containerStart(ctx context.Context, logger *slog.Logger, client *client.Client, reconn io.Writer, containerID string) error {
	containerInfo, err := client.ContainerInspect(context.Background(), containerID)
	if err != nil {
		logger.Error(
			"unable to inspect container info",
			slog.Any("error", err),
		)
		return err
	}

	logger.Debug(
		"container info",
		slog.Any("container_info", containerInfo),
	)

	isEnabled := false
	if enabledRaw, ok := containerInfo.Config.Labels["pipemgr.enable"]; ok {
		if enabledRaw == "true" {
			isEnabled = true
		}
	}

	if !isEnabled {
		return nil
	}

	filterStr := ""
	if filterRaw, ok := containerInfo.Config.Labels["pipemgr.filter"]; ok {
		filterStr = strings.TrimSpace(filterRaw)
	}
	filter, err := regexp.Compile(filterStr)
	if err != nil {
		logger.Error("invalid regex provided to pipemgr.filter", "err", err, "filter", filterStr)
	}

	logger.Info("connecting to logs", "container", containerID)
	opts := ct.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Since:      "0m",
	}
	readCloser, err := client.ContainerLogs(ctx, containerID, opts)
	if err != nil {
		logger.Error("unable to fetch container logs", "err", err)
		return err
	}

	go func() {
		r, w := io.Pipe()
		go func() {
			_, err = stdcopy.StdCopy(w, w, readCloser)
			if err != nil {
				logger.Error("cannot write to pipe topic", "err", err)
			}
		}()
		scanner := bufio.NewScanner(r)
		for {
			for scanner.Scan() {
				line := scanner.Text()
				if filter != nil {
					if !filter.Match([]byte(line)) {
						continue
					}
				}
				logger.Info("piping", "line", line)
				_, err = reconn.Write([]byte(line + "\n"))
				if err != nil {
					logger.Error("could not write to pipe", "err", err)
				}
			}
		}
	}()

	return nil
}

func main() {
	logLevelFlag := flag.String("log-level", "info", "Log level to set for the logger. Can be debug, warn, error, or info")
	remoteHostFlag := flag.String("remote-host", "pipe.pico.sh:22", "The remote host to connect to in the format of host:port")
	remoteHostnameFlag := flag.String("remote-hostname", "pipe.pico.sh", "The remote hostname to verify the host key")
	remoteUserFlag := flag.String("remote-user", "", "The remote user to connect as")
	keyLocationFlag := flag.String("remote-key-location", "/key", "The location on the filesystem of where to access the ssh key")
	keyPassphraseFlag := flag.String("remote-key-passphrase", "", "The passphrase for an encrypted ssh key")
	command := flag.String("command", "pub container-drain -b=false", "The command to run for the remote session")

	flag.Parse()

	var rootLoggerLevel slog.Level

	switch strings.ToLower(*logLevelFlag) {
	case "debug":
		rootLoggerLevel = slog.LevelDebug
	case "warn":
		rootLoggerLevel = slog.LevelWarn
	case "error":
		rootLoggerLevel = slog.LevelError
	default:
		rootLoggerLevel = slog.LevelInfo
	}

	rootLogger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: rootLoggerLevel,
	}))

	slog.SetDefault(rootLogger)

	networks := []string{}

	dockerClient := createDockerClient()
	defer dockerClient.Close()

	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		rootLogger.Error(
			"Unable to get hostname",
			slog.Any("error", err),
		)
	} else {
		info, err := dockerClient.ContainerInspect(context.Background(), hostname)
		if err != nil {
			rootLogger.Error(
				"unable to find networks. Please provide a list to monitor",
				slog.Any("error", err),
			)
			panic(err)
		}

		for netw := range maps.Keys(info.NetworkSettings.Networks) {
			networks = append(networks, strings.ToLower(strings.TrimSpace(netw)))
		}
	}

	info := &pipe.SSHClientInfo{
		RemoteHost:     *remoteHostFlag,
		RemoteHostname: *remoteHostnameFlag,
		RemoteUser:     *remoteUserFlag,
		KeyLocation:    *keyLocationFlag,
		KeyPassphrase:  *keyPassphraseFlag,
	}
	ctx := context.Background()

	reconn := pipe.NewReconnectReadWriteCloser(
		ctx,
		rootLogger,
		info,
		"pipemgr",
		*command,
		100,
		10*time.Millisecond,
	)

	rootLogger = rootLogger.With(
		slog.String("docker", dockerClient.DaemonHost()),
		slog.String("docker_version", dockerClient.ClientVersion()),
	)

	rootLogger.Info("Started pipemgr")

	rootLogger.Debug(
		"Flags",
		"log_level", *logLevelFlag,
		"networks", networks,
		"remote_host", *remoteHostFlag,
		"remote_hostname", *remoteHostnameFlag,
		"remote_user", *remoteUserFlag,
		"key_location", *keyLocationFlag,
		"key_passphrase", *keyPassphraseFlag,
	)

	go func() {
		http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
		})

		err := http.ListenAndServe("localhost:8080", nil)
		if err != nil {
			rootLogger.Error("error with http server", slog.Any("error", err))
		}
	}()

	go func() {
		eventCtx, cancelEventCtx := context.WithCancel(context.Background())
		clientEvents, errs := dockerClient.Events(eventCtx, events.ListOptions{})

		containers, err := dockerClient.ContainerList(context.Background(), ct.ListOptions{})
		if err != nil {
			rootLogger.Error(
				"unable to list container from docker",
				slog.Any("error", err),
			)
		}

		for _, container := range containers {
			logger := rootLogger.With("container", container.ID, "cmd", container.Command)
			err = containerStart(ctx, logger, dockerClient, reconn, container.ID)
			if err != nil {
				logger.Error("cannot write to container", "err", err)
			}
		}

		for {
			select {
			case event := <-clientEvents:
				switch event.Type {
				case events.ContainerEventType:
					logger := slog.With(
						slog.String("event", string(event.Action)),
						slog.String("container_id", event.Actor.ID),
					)
					switch event.Action {
					case events.ActionStart:
						logger.Info("Received start")
						err = containerStart(ctx, logger, dockerClient, reconn, event.Actor.ID)
						if err != nil {
							logger.Error("cannot write to container", "err", err)
							break
						}
					case events.ActionDie:
						logger.Info("Received die")
					default:
						logger.Debug(
							"unhandled container action",
							slog.Any("event_data", event),
						)
					}
				default:
					slog.Debug(
						"unhandled daemon event",
						slog.Any("event_data", event),
					)
				}
			case err := <-errs:
				cancelEventCtx()
				slog.Error(
					"error receiving events from daemon",
					slog.Any("error", err),
				)
				panic(err)
			}
		}
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	for s := range c {
		slog.Info("signal recieved, exiting", slog.Any("signal", s))
		break
	}
	ctx.Done()
}
