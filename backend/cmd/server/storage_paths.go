package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/nianzhibai/91/backend/internal/config"
)

// Management commands use the same path resolver as startup. This command only
// reads configuration; it never creates a database or starts application jobs.
func runStoragePathsCommand(args []string, out, errorOut io.Writer) error {
	flags := flag.NewFlagSet("storage-paths", flag.ContinueOnError)
	flags.SetOutput(errorOut)
	configPath := flags.String("config", applicationConfigPath(), "existing server configuration file")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("storage-paths does not accept positional arguments")
	}
	data, err := os.ReadFile(*configPath)
	if err != nil {
		return fmt.Errorf("read existing configuration: %w", err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return fmt.Errorf("parse configuration: %w", err)
	}
	workingDir, err := os.Getwd()
	if err != nil {
		return err
	}
	storage, err := config.ResolveStoragePaths(cfg.Storage, workingDir)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(struct {
		DataDir string `json:"data_dir"`
		DBPath  string `json:"db_path"`
	}{storage.DataDir, storage.DBPath})
}
