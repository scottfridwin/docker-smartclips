package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"clipfs/config"
	clipfs "clipfs/internal/fs"

	"github.com/hanwen/go-fuse/v2/fs"
)

func getEnv(key, fallback string) string {
	val := os.Getenv(key)
	if val == "" {
		return fallback
	}
	return val
}

func main() {
	configPath := getEnv("CLIPFS_CONFIG", "clips.json")
	mountPath := getEnv("CLIPFS_MOUNT", "/mnt/clipfs")

	clips, err := config.Load(configPath)
	if err != nil {
		log.Fatal(err)
	}

	root := clipfs.NewRoot(clips)

	server, err := fs.Mount(mountPath, root, &fs.Options{})
	if err != nil {
		log.Fatal(err)
	}

	log.Println("ClipFS mounted at", mountPath)

	// graceful shutdown
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sig
		log.Println("Shutting down ClipFS...")
		_ = server.Unmount()
		os.Exit(0)
	}()

	server.Wait()
}
