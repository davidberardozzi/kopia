package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kopia/kopia/fs/repofs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"

	kingpin "gopkg.in/alecthomas/kingpin.v2"
)

const (
	backupMaxDescriptionLength = 1024
)

var (
	backupCommand = app.Command("backup", "Copies local files or directories to backup repository.")

	backupSources = backupCommand.Arg("source", "Files or directories to back up.").ExistingFilesOrDirs()
	backupAll     = backupCommand.Flag("all", "Back-up all directories previously backed up by this user on this computer").Bool()

	backupDescription = backupCommand.Flag("description", "Free-form backup description.").String()

	backupCheckpointInterval      = backupCommand.Flag("checkpoint_interval", "Periodically flush backup (default=30m).").PlaceHolder("TIME").Default("30m").Duration()
	backupCheckpointEveryMB       = backupCommand.Flag("checkpoint_every_mb", "Checkpoint backup after each N megabytes (default=1000).").PlaceHolder("N").Default("1000").Int()
	backupCheckpointUploadLimitMB = backupCommand.Flag("upload_limit_mb", "Stop the backup process after the specified amount of data (in MB) has been uploaded.").PlaceHolder("MB").Default("0").Int()

	backupWriteBack = backupCommand.Flag("async-write", "Perform updates asynchronously.").PlaceHolder("N").Default("0").Int()
)

func runBackupCommand(c *kingpin.ParseContext) error {
	var repoOptions []repo.RepositoryOption

	if *backupWriteBack > 0 {
		repoOptions = append(repoOptions, repo.WriteBack(*backupWriteBack))
	}

	conn := mustOpenConnection(repoOptions...)
	defer conn.Close()

	ctx := context.Background()

	sources := *backupSources
	if *backupAll {
		local, err := getLocalBackupPaths(conn.SnapshotManager)
		if err != nil {
			return err
		}
		sources = append(sources, local...)
	}

	if len(sources) == 0 {
		return fmt.Errorf("No backup sources.")
	}

	for _, backupDirectory := range sources {
		conn.Repository.Stats.Reset()
		log.Printf("Backing up %v", backupDirectory)
		dir, err := filepath.Abs(backupDirectory)
		if err != nil {
			return fmt.Errorf("invalid source: '%s': %s", backupDirectory, err)
		}

		sourceInfo := &snapshot.SourceInfo{Path: filepath.Clean(dir), Host: getHostName(), UserName: getUserName()}

		if len(*backupDescription) > backupMaxDescriptionLength {
			return fmt.Errorf("description too long")
		}

		previous, err := conn.SnapshotManager.ListSnapshots(sourceInfo, 1)
		if err != nil {
			return fmt.Errorf("error listing previous backups: %v", err)
		}

		var oldManifest *snapshot.Manifest

		if len(previous) > 0 {
			oldManifest = previous[0]
		}

		localEntry := mustGetLocalFSEntry(sourceInfo.Path)
		if err != nil {
			return err
		}

		manifest, err := repofs.Upload(
			ctx,
			conn.Repository,
			localEntry,
			sourceInfo,
			oldManifest,
			&uploadProgress{})
		if err != nil {
			return err
		}

		handleID, err := conn.Vault.SaveObjectID(manifest.RootObjectID)
		if err != nil {
			return err
		}

		uniqueID := make([]byte, 8)
		rand.Read(uniqueID)
		fileID := fmt.Sprintf("B%v.%08x.%x", sourceInfo.HashString(), math.MaxInt64-manifest.StartTime.UnixNano(), uniqueID)
		manifest.Handle = handleID
		manifest.Description = *backupDescription

		err = saveBackupManifest(conn.Vault, fileID, manifest)
		if err != nil {
			return fmt.Errorf("cannot save manifest: %v", err)
		}

		log.Printf("Root: %v", manifest.RootObjectID.String())
		log.Printf("Hash Cache: %v", manifest.HashCacheID.String())
		log.Printf("Key: %v", handleID)

		b, _ := json.MarshalIndent(&manifest, "", "  ")
		log.Printf("%s", string(b))
	}

	return nil
}

func getLocalBackupPaths(mgr *snapshot.Manager) ([]string, error) {
	h := getHostName()
	u := getUserName()
	log.Printf("Looking for previous backups of '%v@%v'...", u, h)

	sources, err := mgr.Sources()
	if err != nil {
		return nil, err
	}

	var result []string

	for _, src := range sources {
		if src.Host == h && src.UserName == u {
			result = append(result, src.Path)
		}
	}

	return result, nil
}

func hashObjectID(oid string) string {
	h := sha256.New()
	io.WriteString(h, oid)
	sum := h.Sum(nil)
	foldLen := 16
	for i := foldLen; i < len(sum); i++ {
		sum[i%foldLen] ^= sum[i]
	}
	return hex.EncodeToString(sum[0:foldLen])
}

func getUserOrDefault(userName string) string {
	if userName != "" {
		return userName
	}

	return getUserName()
}

func getUserName() string {
	currentUser, err := user.Current()
	if err != nil {
		log.Fatalf("Cannot determine current user: %s", err)
	}

	u := currentUser.Username
	if runtime.GOOS == "windows" {
		if p := strings.Index(u, "\\"); p >= 0 {
			// On Windows ignore domain name.
			u = u[p+1:]
		}
	}

	return u
}

func getHostNameOrDefault(hostName string) string {
	if hostName != "" {
		return hostName
	}

	return getHostName()
}

func getHostName() string {
	hostname, err := os.Hostname()
	if err != nil {
		log.Fatalf("Unable to determine hostname: %s", err)
	}

	// Normalize hostname.
	hostname = strings.ToLower(strings.Split(hostname, ".")[0])

	return hostname
}

func init() {
	backupCommand.Action(runBackupCommand)
}
