package main

import (
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"clipfs/config"
	clipfs "clipfs/internal/fs"

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

	f, err := os.Open("/dev/fuse")
	if err != nil {
		log.Fatalf("FATAL: cannot access /dev/fuse: %v", err)
	}
	f.Close()
	log.Println("DEBUG: /dev/fuse accessible at startup")

	configPath := getEnv("CLIPFS_CONFIG", "clips.json")
	mountPath := getEnv("CLIPFS_MOUNT", "/mnt/clipfs")
	mediaPrefix := getEnv("CLIPFS_MEDIA", "")

	clips, err := config.Load(configPath, mediaPrefix)
	if err != nil {
		log.Fatal(err)
	}

	uid := getEnvInt("PUID", 0)
	gid := getEnvInt("PGID", 0)

	root := clipfs.NewRoot(clips, uid, gid)

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

	server.Wait()
}
