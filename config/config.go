package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Remote struct {
		SSHKey  string         `toml:"ssh_key"`
		SSHHost string         `toml:"ssh_host"`
		Policy  map[string]int `toml:"policy"`
		Root    string         `toml:"root"`
	} `toml:"remote"`
	Local struct {
		Policy map[string]int `toml:"policy"`
		Root   string         `toml:"root"`
	}
}

var pathHierarchy = []string{
	"/etc/backupd.toml",
	"/usr/local/etc/backupd.toml",
	"/opt/local/etc/backupd.toml",
	"/Library/Application Support/co.monks.backupd/backupd.toml",
}

func Load() (*Config, error) {
	for _, path := range pathHierarchy {
		f, err := os.Open(path)
		if err != nil && os.IsNotExist(err) {
			continue
		} else if err != nil {
			return nil, err
		}

		defer f.Close()

		dec := toml.NewDecoder(f)
		var conf Config
		if _, err := dec.Decode(&conf); err != nil {
			return nil, fmt.Errorf("decoding '%s': %w", path, err)
		}

		return &conf, nil
	}

	return nil, fmt.Errorf("no config file exists {%s}", strings.Join(pathHierarchy, ", "))
}
