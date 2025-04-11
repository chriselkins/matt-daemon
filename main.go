package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"
	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

const appName = "MATT-Daemon"

type MoveTask struct {
	Trigger   string   `yaml:"trigger"`
	Source    string   `yaml:"source"`
	Target    string   `yaml:"target"`
	User      string   `yaml:"user"`
	Group     string   `yaml:"group"`
	FileMode  string   `yaml:"file_mode"` // e.g. "0640"
	DirMode   string   `yaml:"dir_mode"`  // e.g. "0750"
	Overwrite bool     `yaml:"overwrite"`
	Pre       []string `yaml:"pre"`
	Post      []string `yaml:"post"`
}

type Config struct {
	MoveTasks []MoveTask `yaml:"move_tasks"`
}

var (
	configPath = flag.String("config", fmt.Sprintf("/etc/%s/config.yaml", appName), "Path to YAML config file")
)

func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags)

	log.Printf("📡 Starting %s daemon...\n", appName)

	cfg, err := loadConfig(*configPath)

	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go handleSignals(cancel)

	triggerMap := make(map[string]MoveTask)
	watchDirs := make(map[string]struct{})

	for _, task := range cfg.MoveTasks {
		triggerMap[task.Trigger] = task
		watchDirs[filepath.Dir(task.Trigger)] = struct{}{}
	}

	watcher, err := fsnotify.NewWatcher()

	if err != nil {
		log.Fatalf("Failed to create inotify watcher: %v", err)
	}

	defer watcher.Close()

	for dir := range watchDirs {
		if err := watcher.Add(dir); err != nil {
			log.Fatalf("Failed to watch directory %s: %v", dir, err)
		}
		log.Printf("📂 Watching directory: %s", dir)
	}

	// Notify systemd we're ready
	daemon.SdNotify(false, daemon.SdNotifyReady)
	log.Println("✅ Ready and waiting for trigger files...")

	for {
		select {
		case event := <-watcher.Events:
			if event.Op&fsnotify.Create == fsnotify.Create {
				trigger := event.Name
				task, ok := triggerMap[trigger]
				if ok {
					log.Printf("🟢 Trigger detected: %s", trigger)
					time.Sleep(500 * time.Millisecond) // allow time to settle
					if err := processMoveNow(task); err != nil {
						log.Printf("Error processing task %s: %v", trigger, err)
					}
				}
			}
		case err := <-watcher.Errors:
			log.Printf("Watcher error: %v", err)
		case <-ctx.Done():
			log.Printf("🛑 Shutting down %s.\n", appName)
			return
		}
	}
}

func handleSignals(cancel context.CancelFunc) {
	c := make(chan os.Signal, 1)

	signal.Notify(c, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGHUP)

	for sig := range c {
		switch sig {
		case syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT:
			log.Printf("⚠️ Received signal: %s — shutting down...", sig)
			cancel()
			return
		case syscall.SIGHUP:
			log.Println("🔁 Received SIGHUP — exiting so systemd restarts with fresh config")
			os.Exit(0) // systemd will restart us with new config
		}
	}
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func processMoveNow(task MoveTask) error {
	if len(task.Pre) > 0 {
		if err := runHooks("pre", task.Pre); err != nil {
			log.Printf("⛔ Pre-hook failed: %v", err)
			return err
		}
	}

	if err := os.Rename(task.Trigger, task.Trigger+".processing"); err != nil {
		return fmt.Errorf("renaming trigger to processing: %w", err)
	}

	entries, err := os.ReadDir(task.Source)
	if err != nil {
		return fmt.Errorf("reading source dir: %w", err)
	}

	for _, entry := range entries {
		// skip the trigger file and the processing file itself
		if entry.Name() == filepath.Base(task.Trigger)+".processing" ||
			entry.Name() == filepath.Base(task.Trigger) {
			continue
		}

		srcPath := filepath.Join(task.Source, entry.Name())
		destPath := filepath.Join(task.Target, entry.Name())

		if stat, err := os.Stat(destPath); err == nil {
			if !task.Overwrite {
				log.Printf("Skipping %s: destination exists", destPath)
				continue
			}
			if stat.IsDir() {
				if err := os.RemoveAll(destPath); err != nil {
					log.Printf("Failed to remove existing dir: %v", err)
					continue
				}
			} else {
				if err := os.Remove(destPath); err != nil {
					log.Printf("Failed to remove existing file: %v", err)
					continue
				}
			}
		}

		err := os.Rename(srcPath, destPath)
		if err != nil && isCrossDevice(err) {
			if entry.IsDir() {
				err = copyDir(srcPath, destPath)
			} else {
				err = copyFile(srcPath, destPath)
			}
			if err == nil {
				_ = os.RemoveAll(srcPath)
			}
		}
		if err != nil {
			log.Printf("Failed to move %s to %s: %v", srcPath, destPath, err)
			continue
		}

		log.Printf("Moved: %s → %s", srcPath, destPath)

		if entry.IsDir() {
			_ = applyRecursivePermissions(destPath, task)
		} else {
			_ = applyOwnershipAndPermissions(destPath, false, task)
		}
	}

	if len(task.Post) > 0 {
		if err := runHooks("post", task.Post); err != nil {
			log.Printf("⚠️ Post-hook failed: %v", err)
			// Don't abort the task; just log
		}
	}

	if err := os.Remove(task.Trigger); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing trigger: %w", err)
	}

	if err := os.Remove(task.Trigger + ".processing"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing trigger: %w", err)
	}

	return nil
}

func isCrossDevice(err error) bool {
	return strings.Contains(err.Error(), "cross-device link")
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relPath, _ := filepath.Rel(src, path)
		destPath := filepath.Join(dst, relPath)
		if info.IsDir() {
			return os.MkdirAll(destPath, info.Mode())
		}
		return copyFile(path, destPath)
	})
}

func applyOwnershipAndPermissions(path string, isDir bool, task MoveTask) error {
	var modeStr string
	if isDir {
		modeStr = task.DirMode
	} else {
		modeStr = task.FileMode
	}

	if modeStr != "" {
		modeVal, err := strconv.ParseUint(modeStr, 8, 32)
		if err == nil {
			_ = os.Chmod(path, os.FileMode(modeVal))
		}
	}

	uid, gid := -1, -1
	if task.User != "" {
		if u, err := user.Lookup(task.User); err == nil {
			uid, _ = strconv.Atoi(u.Uid)
		}
	}
	if task.Group != "" {
		if g, err := user.LookupGroup(task.Group); err == nil {
			gid, _ = strconv.Atoi(g.Gid)
		}
	}
	if uid != -1 || gid != -1 {
		_ = os.Chown(path, uid, gid)
	}

	return nil
}

func applyRecursivePermissions(root string, task MoveTask) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return applyOwnershipAndPermissions(path, info.IsDir(), task)
	})
}

func runHooks(name string, cmds []string) error {
	for _, cmdLine := range cmds {
		log.Printf("▶️  Running %s hook: %s", name, cmdLine)

		cmd := exec.Command("/bin/sh", "-c", cmdLine)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s hook failed: %w", name, err)
		}
	}

	return nil
}
