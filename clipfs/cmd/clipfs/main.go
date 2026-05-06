package main

import (
	"log"
	"os"

	"clipfs/config"
	clipfs "clipfs/internal/fs"

	"github.com/hanwen/go-fuse/v2/fs"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Usage: clipfs <mountpoint>")
	}

	mountpoint := os.Args[1]

	clips, err := config.Load("clips.json")
	if err != nil {
		log.Fatal(err)
	}

	root := clipfs.NewRoot(clips)

	server, err := fs.Mount(mountpoint, root, &fs.Options{})
	if err != nil {
		log.Fatal(err)
	}

	log.Println("Mounted at", mountpoint)
	server.Wait()
}
