package cmd

import (
	"encoding/json"
	ice "github.com/pion/ice/v2"
	"github.com/wiretrustee/wiretrustee/connection"
	"io/ioutil"
	"os"
	"path/filepath"
)

// Config Configuration type
type Config struct {
	// Wireguard private key of local peer
	PrivateKey   string
	Peers        []connection.Peer
	StunTurnURLs []*ice.URL
	// host:port of the signal server
	SignalAddr     string
	WgAddr         string
	WgIface        string
	IFaceBlackList []string
}

//Write writes configPath to a file
func (cfg *Config) Write(path string) error {

	if path == defaultConfigPath {
		configDir := filepath.Dir(path)
		err := os.MkdirAll(configDir, 0750)
		if err != nil {
			return err
		}
	}

	bs, err := json.Marshal(cfg)
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(path, bs, 0600)
	if err != nil {
		return err
	}

	return nil
}

//Read reads configPath from a file
func Read(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	bs, err := ioutil.ReadAll(f)
	if err != nil {
		return nil, err
	}

	var cfg Config
	err = json.Unmarshal(bs, &cfg)
	if err != nil {
		return nil, err
	}

	return &cfg, nil
}
