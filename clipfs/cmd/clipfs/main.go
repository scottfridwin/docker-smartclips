package main

import (
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"clipfs/config"
	"clipfs/internal/cache"
	clipfs "clipfs/internal/fs"

	"github.com/fsnotify/fsnotify"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func getEnv(key, fallback string) string {
	val := os.Getenv(key)
	if val == "" {
		return fallback
	}
	return val
}

func getEnvInt(key string, fallback int) int {
	v := os.Getenv(key)
	i, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return i
}

func main() {

	configPath := getEnv("CLIPFS_CONFIG", "clips.json")
	mountPath := getEnv("CLIPFS_MOUNT", "/mnt/clipfs")
	mediaPrefix := getEnv("CLIPFS_MEDIA", "")

	clips, err := config.Load(configPath, mediaPrefix)
	if err != nil {
		log.Fatal(err)
	}

	uid := os.Getuid()
	gid := os.Getgid()

	cachePath := getEnv("CLIPFS_CACHE_DIR", "/tmp/clipfs-cache")
	cacheMaxMB := getEnvInt("CLIPFS_CACHE_MAX_MB", 1024)

	diskCache, err := cache.New(cachePath, cacheMaxMB)
	if err != nil {
		log.Fatalf("Failed to initialize disk cache: %v", err)
	}
	log.Printf("Disk cache: %s (max %d MB)", cachePath, cacheMaxMB)

	root := clipfs.NewRoot(clips, uid, gid, diskCache)

	// ✅ IMPORTANT: use fs.Mount but DO NOT rely on extra heuristic flags
	server, err := fs.Mount(mountPath, root, &fs.Options{
		MountOptions: fuse.MountOptions{
			AllowOther: true,
			FsName:     "clipfs",
			Name:       "clipfs",
		},
	})

	if err != nil {
		log.Fatal(err)
	}
	defer server.Unmount()

	log.Println("ClipFS mounted at", mountPath)

	// graceful shutdown
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sig
		log.Println("Shutting down ClipFS...")
		_ = server.Unmount()
	}()

	// Watch config file for hot-reload
	go watchConfig(configPath, mediaPrefix, root)

	server.Wait()
}

func watchConfig(configPath, mediaPrefix string, root *clipfs.Root) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("Warning: could not start config watcher: %v", err)
		return
	}
	defer watcher.Close()

	if err := watcher.Add(configPath); err != nil {
		log.Printf("Warning: could not watch %s: %v", configPath, err)
		return
	}

	log.Printf("Watching %s for changes", configPath)

	// Debounce: editors may trigger multiple events for one save
	var debounce *time.Timer

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
				if debounce != nil {
					debounce.Stop()
				}
				debounce = time.AfterFunc(500*time.Millisecond, func() {
					clips, err := config.Load(configPath, mediaPrefix)
					if err != nil {
						log.Printf("Reload failed: %v", err)
						return
					}
					root.Reload(clips)
				})
			}
			// If the file is removed and recreated (e.g., Docker config mount),
			// re-add the watch
			if event.Has(fsnotify.Remove) {
				watcher.Add(configPath)
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("Config watcher error: %v", err)
		}
	}
}
