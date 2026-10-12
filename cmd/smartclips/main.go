// Command smartclips serves virtual video clips over FUSE and hosts the clip manager UI.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/scottfridwin/docker-smartclips/internal/clipfs"
	"github.com/scottfridwin/docker-smartclips/internal/config"
	"github.com/scottfridwin/docker-smartclips/internal/engine"
	"github.com/scottfridwin/docker-smartclips/internal/privs"
	"github.com/scottfridwin/docker-smartclips/internal/ui"
)

var version = "dev"

const usage = `usage: smartclips [mount|ui|healthcheck|version]

  mount        serve the clips over FUSE (default)
  ui           serve the clip manager web UI
  healthcheck  exit 0 when the running service is healthy
`

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a number", key, v)
	}
	return n, nil
}

func main() {
	level := slog.LevelInfo
	if strings.EqualFold(os.Getenv("SMARTCLIPS_LOG_LEVEL"), "debug") {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cmd := "mount"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "mount":
		err = runMount(log)
	case "ui":
		err = runUI(log)
	case "healthcheck":
		err = healthcheck()
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Error(cmd+" failed", "err", err)
		os.Exit(1)
	}
}

func runMount(log *slog.Logger) error {
	cfgPath := env("SMARTCLIPS_CONFIG", "/data/clips.json")
	mountPoint := env("SMARTCLIPS_MOUNT", "/mnt/smartclips/library")
	media := env("SMARTCLIPS_MEDIA", "/media")
	cache := env("SMARTCLIPS_CACHE_DIR", "/cache")
	workers, err := envInt("SMARTCLIPS_WORKERS", 2)
	if err != nil {
		return err
	}
	recheck, err := envInt("SMARTCLIPS_RECHECK_MINUTES", 15)
	if err != nil {
		return err
	}

	asRoot := os.Geteuid() == 0
	uid, gid := os.Getuid(), os.Getgid()
	if asRoot {
		// Root is only needed to mount; everything else runs as PUID:PGID.
		if uid, err = envInt("PUID", -1); err != nil {
			return err
		}
		if gid, err = envInt("PGID", -1); err != nil {
			return err
		}
		if uid <= 0 || gid <= 0 {
			return errors.New("running as root requires non-root PUID and PGID to switch to after mounting")
		}
	}
	log.Info("starting", "version", version, "config", cfgPath, "mount", mountPoint, "media", media,
		"cache", cache, "uid", uid, "gid", gid)

	file, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	var eng *engine.Engine
	root := clipfs.New(uid, gid, func(p string) { eng.Invalidate(p) })
	eng = engine.New(engine.Options{
		MediaRoot: media, CacheDir: cache, Workers: workers,
		RecheckPeriod: time.Duration(recheck) * time.Minute,
	}, root, log)
	// Publish everything already in the index before mounting, so the library never
	// appears empty to Jellyfin while it starts.
	eng.Apply(file)
	log.Info("clips loaded", "configured", len(file.Clips), "ready", root.Len())

	if asRoot && staleFUSE(mountPoint) {
		// A previous instance that died leaves a disconnected FUSE mount; detach it. Because
		// it sits below the shared bind mount, the detach propagates to the host too.
		log.Info("detaching stale mount", "mount", mountPoint)
		_ = syscall.Unmount(mountPoint, syscall.MNT_DETACH)
	}
	mkdir := func() error { return os.MkdirAll(mountPoint, 0o755) }
	if asRoot {
		// The parent belongs to the service user, and root holds no DAC_OVERRIDE.
		err = privs.As(uid, gid, mkdir)
	} else {
		err = mkdir()
	}
	if err != nil {
		return err
	}
	if isMountPoint(mountPoint) {
		log.Warn("SMARTCLIPS_MOUNT is itself a mount point; bind-mount its parent directory with " +
			"rshared propagation instead, or a crashed instance's mount can't be recovered")
	}
	hour, negative := time.Hour, 10*time.Second
	server, err := fs.Mount(mountPoint, root, &fs.Options{
		MountOptions: fuse.MountOptions{
			AllowOther:           true,
			FsName:               "smartclips",
			Name:                 "smartclips",
			DirectMount:          asRoot,
			DirectMountStrict:    asRoot,
			MaxReadAhead:         1 << 20,
			DisableXAttrs:        true,
			IgnoreSecurityLabels: true,
			Options:              []string{"ro"},
		},
		EntryTimeout:    &hour,
		AttrTimeout:     &hour,
		NegativeTimeout: &negative,
	})
	if err != nil {
		return fmt.Errorf("mount %s: %w", mountPoint, err)
	}
	if asRoot {
		if err := privs.Drop(uid, gid); err != nil {
			_ = server.Unmount()
			return fmt.Errorf("dropping privileges: %w", err)
		}
	}
	log.Info("mounted", "mount", mountPoint)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	eng.Start(ctx)
	go watchConfig(ctx, log, cfgPath, eng, time.Duration(recheck)*time.Minute)

	go func() {
		<-ctx.Done()
		log.Info("shutting down")
		if asRoot {
			if err := privs.Elevate(); err != nil {
				log.Error("regaining root to unmount", "err", err)
			}
		}
		if err := server.Unmount(); err != nil {
			log.Warn("unmount failed; detaching lazily", "err", err)
			_ = syscall.Unmount(mountPoint, syscall.MNT_DETACH)
		}
	}()
	server.Wait()
	return nil
}

// watchConfig reloads the clips file when it changes (and periodically, as a fallback).
func watchConfig(ctx context.Context, log *slog.Logger, path string, eng *engine.Engine, every time.Duration) {
	reload := func(reason string) {
		f, err := config.Load(path)
		if err != nil {
			log.Error("clips file not reloaded", "reason", reason, "err", err)
			return
		}
		log.Info("clips file reloaded", "reason", reason, "clips", len(f.Clips))
		eng.Apply(f)
	}
	var events <-chan fsnotify.Event
	w, err := fsnotify.NewWatcher()
	if err == nil {
		defer func() { _ = w.Close() }()
		// Watch the directory: saves replace the file, which would drop a file watch.
		if err = w.Add(filepath.Dir(path)); err == nil {
			events = w.Events
		}
	}
	if err != nil {
		log.Warn("config watcher unavailable; relying on periodic reload", "err", err)
	}
	if every <= 0 {
		every = 15 * time.Minute
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	var debounce <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if filepath.Base(ev.Name) == filepath.Base(path) {
				debounce = time.After(500 * time.Millisecond)
			}
		case <-debounce:
			debounce = nil
			reload("changed")
		case <-tick.C:
			reload("periodic")
		}
	}
}

func runUI(log *slog.Logger) error {
	listen := env("SMARTCLIPS_UI_LISTEN", ":8080")
	srv := &http.Server{
		Addr: listen,
		Handler: ui.New(ui.Options{
			ConfigPath: env("SMARTCLIPS_CONFIG", "/data/clips.json"),
			MediaRoot:  env("SMARTCLIPS_MEDIA", "/media"),
			StatusPath: filepath.Join(env("SMARTCLIPS_CACHE_DIR", "/cache"), "status.json"),
		}, log),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("ui listening", "version", version, "addr", listen)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

const fuseSuperMagic = 0x65735546

// staleFUSE reports whether path is a FUSE mount (connected or not).
func staleFUSE(path string) bool {
	var st syscall.Statfs_t
	err := syscall.Statfs(path, &st)
	return errors.Is(err, syscall.ENOTCONN) || (err == nil && st.Type == fuseSuperMagic)
}

// isMountPoint reports whether path is on a different device than its parent.
func isMountPoint(path string) bool {
	var self, parent syscall.Stat_t
	if syscall.Stat(path, &self) != nil || syscall.Stat(filepath.Dir(path), &parent) != nil {
		return false
	}
	return self.Dev != parent.Dev
}

// healthcheck inspects the service running as PID 1 in this container.
func healthcheck() error {
	cmdline, _ := os.ReadFile("/proc/1/cmdline")
	args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	if len(args) > 1 && args[1] == "ui" {
		addr := env("SMARTCLIPS_UI_LISTEN", ":8080")
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return err
		}
		client := http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("healthz returned %s", resp.Status)
		}
		return nil
	}
	// A dead FUSE server makes statfs hang or fail with ENOTCONN.
	done := make(chan error, 1)
	go func() {
		var st syscall.Statfs_t
		err := syscall.Statfs(env("SMARTCLIPS_MOUNT", "/mnt/smartclips/library"), &st)
		if err == nil && st.Type != fuseSuperMagic {
			err = errors.New("smartclips is not mounted")
		}
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		return errors.New("mount is not responding")
	}
}
