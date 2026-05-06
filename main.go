package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antoniomika/syncmap"
	ct "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/picosh/utils/pipe"
)

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func countMap[K comparable, V any](m *syncmap.Map[K, V]) int {
	count := 0
	m.Range(func(_ K, _ V) bool {
		count++
		return true
	})
	return count
}

type LogHandler struct {
	Ctx            context.Context
	Cancel         context.CancelFunc
	Logger         *slog.Logger
	ContainerID    string
	LogClients     *LogClients
	LinesForwarded atomic.Int64
	LinesFiltered  atomic.Int64
	WriteErrors    atomic.Int64
	ReconnectCount atomic.Int64
}

func (lh *LogHandler) Stats() (linesForwarded, linesFiltered, writeErrors, reconnects int64) {
	return lh.LinesForwarded.Load(), lh.LinesFiltered.Load(), lh.WriteErrors.Load(), lh.ReconnectCount.Load()
}

func (lh *LogHandler) Handle() {
	for {
		select {
		case <-lh.Ctx.Done():
			lh.Logger.Info("context cancelled, exiting handler")
			return
		default:
			containerInfo, err := lh.LogClients.DockerClient.ContainerInspect(lh.Ctx, lh.ContainerID)
			if err != nil {
				lh.Logger.Error(
					"unable to inspect container info",
					slog.Any("error", err),
				)
				lh.LogClients.RemoveContainer(lh.ContainerID)
				return
			}

			lh.Logger.Debug(
				"container info",
				"container_name", containerInfo.Name,
				"image", containerInfo.Config.Image,
				"state", containerInfo.State.Status,
			)

			isEnabled := false
			if enabledRaw, ok := containerInfo.Config.Labels["pipemgr.enable"]; ok {
				if enabledRaw == "true" {
					isEnabled = true
				}
			}

			if !isEnabled {
				lh.Logger.Info(
					"skipping container - pipemgr.enable label not set or not true",
					"label_value", containerInfo.Config.Labels["pipemgr.enable"],
				)
				lh.LogClients.RemoveContainer(lh.ContainerID)
				return
			}

			filterStr := ""
			if filterRaw, ok := containerInfo.Config.Labels["pipemgr.filter"]; ok {
				filterStr = strings.TrimSpace(filterRaw)
			}

			var filter *regexp.Regexp
			if filterStr != "" {
				filter, err = regexp.Compile(filterStr)
				if err != nil {
					lh.Logger.Error("invalid regex provided to pipemgr.filter", "err", err, "filter", filterStr)
					// Don't return - continue without filter
				}
			}

			// Track reconnect attempts
			reconnects := lh.ReconnectCount.Add(1)

			lh.Logger.Info(
				"connecting to container logs",
				"container", lh.ContainerID,
				"container_name", containerInfo.Name,
				"filter", filterStr,
				"reconnect_attempt", reconnects,
			)

			opts := ct.LogsOptions{
				ShowStdout: true,
				ShowStderr: true,
				Follow:     true,
				Since:      "0m",
			}

			readCloser, err := lh.LogClients.DockerClient.ContainerLogs(lh.Ctx, lh.ContainerID, opts)
			if err != nil {
				lh.Logger.Error("unable to fetch container logs", "err", err, "container", lh.ContainerID)
				time.Sleep(5 * time.Second)
				continue
			}

			r, w := io.Pipe()

			var wg sync.WaitGroup

			wg.Add(1)
			go func() {
				defer func() {
					w.Close()
					wg.Done()
				}()

				_, err := stdcopy.StdCopy(w, w, readCloser)
				if err != nil {
					lh.Logger.Error("cannot split multiplex stream", "err", err, "container", lh.ContainerID)
				}
			}()

			wg.Add(1)
			go func() {
				defer func() {
					readCloser.Close()
					wg.Done()
				}()

				scanner := bufio.NewScanner(r)
				lineCount := int64(0)
				for scanner.Scan() {
					line := scanner.Text()
					lineCount++

					if filter != nil {
						if !filter.Match([]byte(line)) {
							lh.LinesFiltered.Add(1)
							// Log filtered lines at debug level every 1000 lines
							if lineCount%1000 == 0 {
								lh.Logger.Debug("log lines filtered", "lines_processed", lineCount, "sample_line", line[:min(80, len(line))])
							}
							continue
						}
					}

					lh.Logger.Debug("forwarding log line", "line", line)

					_, err := lh.LogClients.Pipe.Write([]byte(line + "\n"))
					if err != nil {
						lh.WriteErrors.Add(1)
						lh.Logger.Error(
							"could not write to pipe",
							"err", err,
							"container", lh.ContainerID,
							"lines_forwarded_before_error", lineCount,
						)
					} else {
						lh.LinesForwarded.Add(1)
					}
				}

				if err := scanner.Err(); err != nil {
					lh.Logger.Error("error reading from container logs", "err", err, "lines_processed", lineCount)
				} else {
					lh.Logger.Info(
						"log stream closed",
						"container", lh.ContainerID,
						"lines_forwarded", lineCount,
					)
				}
			}()

			wg.Wait()

			lh.Logger.Info(
				"disconnected from logs, retrying in 5s",
				"container", lh.ContainerID,
				"reconnect_attempt", reconnects,
			)
			time.Sleep(5 * time.Second)
		}
	}
}

func (lh *LogHandler) Close() {
	lh.Cancel()
}

type LogClients struct {
	Ctx          context.Context
	Logger       *slog.Logger
	DockerClient *client.Client
	Pipe         *pipe.ReconnectReadWriteCloser
	Clients      *syncmap.Map[string, *LogHandler]
}

func (lc *LogClients) AddContainer(ctx context.Context, logger *slog.Logger, containerID string) *LogHandler {
	ctx, cancel := context.WithCancel(ctx)

	handler, ok := lc.Clients.LoadOrStore(containerID, &LogHandler{
		Ctx:         ctx,
		Cancel:      cancel,
		Logger:      logger,
		ContainerID: containerID,
		LogClients:  lc,
	})

	if !ok {
		logger.Info("added container handler", "container_id", containerID, "total_containers", countMap(lc.Clients))
		go handler.Handle()
	} else {
		logger.Debug("container handler already exists", "container_id", containerID)
	}

	return handler
}

func (lc *LogClients) RemoveContainer(containerID string) {
	handler, ok := lc.Clients.Load(containerID)
	if ok {
		lf, lfr, we, rc := handler.Stats()
		lc.Logger.Info(
			"removing container",
			"container_id", containerID,
			"lines_forwarded", lf,
			"lines_filtered", lfr,
			"write_errors", we,
			"reconnects", rc,
			"remaining_containers", countMap(lc.Clients)-1,
		)
		handler.Close()
		lc.Clients.Delete(containerID)
	} else {
		lc.Logger.Debug("attempted to remove unknown container", "container_id", containerID)
	}
}

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

func main() {
	logLevelFlag := flag.String("log-level", "info", "Log level to set for the logger. Can be debug, warn, error, or info")
	remoteHostFlag := flag.String("remote-host", "pipe.pico.sh:22", "The remote host to connect to in the format of host:port")
	remoteHostnameFlag := flag.String("remote-hostname", "pipe.pico.sh", "The remote hostname to verify the host key")
	remoteUserFlag := flag.String("remote-user", "", "The remote user to connect as")
	keyLocationFlag := flag.String("remote-key-location", "/key", "The location on the filesystem of where to access the ssh key")
	keyCertLocationFlag := flag.String("remote-key-cert-location", "/key-cert.pub", "The location on the filesystem of where to access the ssh key cert")
	keyPassphraseFlag := flag.String("remote-key-passphrase", "", "The passphrase for an encrypted ssh key")
	networkNames := flag.String("networks", "", "The networks to monitor for containers")
	httpAddress := flag.String("http-address", "localhost:8080", "The address to bind the http server to")
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if *networkNames != "" {
		networks = strings.Split(strings.ToLower(*networkNames), ",")
	} else {
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
	}

	info := &pipe.SSHClientInfo{
		RemoteHost:          *remoteHostFlag,
		RemoteHostname:      *remoteHostnameFlag,
		RemoteUser:          *remoteUserFlag,
		KeyLocation:         *keyLocationFlag,
		CertificateLocation: *keyCertLocationFlag,
		KeyPassphrase:       *keyPassphraseFlag,
	}

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
		"key_cert_location", *keyCertLocationFlag,
		"key_passphrase", *keyPassphraseFlag,
	)

	logClients := &LogClients{
		Ctx:          ctx,
		Logger:       rootLogger,
		DockerClient: dockerClient,
		Pipe:         reconn,
		Clients:      syncmap.New[string, *LogHandler](),
	}

	go func() {
		http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
		})

		http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			var stats []map[string]interface{}
			logClients.Clients.Range(func(id string, handler *LogHandler) bool {
				lf, lfr, we, rc := handler.Stats()
				stats = append(stats, map[string]interface{}{
					"container_id":    id,
					"lines_forwarded": lf,
					"lines_filtered":  lfr,
					"write_errors":    we,
					"reconnects":      rc,
				})
				return true
			})
			result, err := json.Marshal(map[string]interface{}{
				"active_containers": len(stats),
				"containers":        stats,
			})
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Write(result)
		})

		err := http.ListenAndServe(*httpAddress, nil)
		if err != nil {
			rootLogger.Error("error with http server", slog.Any("error", err))
			cancel()
		}
	}()

	clientEvents, errs := dockerClient.Events(ctx, events.ListOptions{})

	containers, err := dockerClient.ContainerList(ctx, ct.ListOptions{})
	if err != nil {
		rootLogger.Error(
			"unable to list container from docker",
			slog.Any("error", err),
		)
	} else {
		rootLogger.Info("scanning existing containers", "total_found", len(containers))
		for _, container := range containers {
			logger := rootLogger.With(
				"container", container.ID,
				"container_name", container.Names[0],
				"image", container.Image,
				"state", container.State,
			)
			logger.Info("found existing container", "container_id", container.ID)
			logClients.AddContainer(ctx, logger, container.ID)
		}
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

forLoop:
	for {
		select {
		case event := <-clientEvents:
			switch event.Type {
			case events.ContainerEventType:
				logger := slog.With(
					slog.String("event_type", string(event.Type)),
					slog.String("event_action", string(event.Action)),
					slog.String("container_id", event.Actor.ID),
					"container_name", event.Actor.Attributes["name"],
					"image", event.Actor.Attributes["image"],
				)
				switch event.Action {
				case events.ActionStart:
					logger.Info("container started - adding handler")
					logClients.AddContainer(ctx, logger, event.Actor.ID)
				case events.ActionDie:
					logger.Info("container stopped - removing handler")
					logClients.RemoveContainer(event.Actor.ID)
				case events.ActionRestart:
					logger.Info("container restarted - removing and re-adding handler")
					logClients.RemoveContainer(event.Actor.ID)
					time.Sleep(1 * time.Second) // brief delay to let container restart
					logClients.AddContainer(ctx, logger, event.Actor.ID)
				case events.ActionDestroy:
					logger.Info("container destroyed - removing handler")
					logClients.RemoveContainer(event.Actor.ID)
				default:
					logger.Debug(
						"unhandled container action",
						slog.Any("event_data", event),
					)
				}
			default:
				slog.Debug(
					"unhandled daemon event",
					"event_type", string(event.Type),
					"event_action", string(event.Action),
				)
			}
		case err := <-errs:
			slog.Error(
				"error receiving events from daemon",
				slog.Any("error", err),
			)
			break forLoop
		case s := <-c:
			slog.Info("signal received, exiting", slog.Any("signal", s))
			break forLoop
		case <-ctx.Done():
			slog.Info("context cancelled, exiting")
			break forLoop
		}
	}
}
