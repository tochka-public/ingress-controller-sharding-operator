package config

import (
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type ConfigType struct {
	ShardedIngress          map[string]Shard `yaml:"shardedIngress"`
	ShardedHTTPProxy        map[string]Shard `yaml:"shardedHTTPProxy"`
	TerminationPeriod       time.Duration    `yaml:"terminationPeriod"`
	ShardUpdateCooldown     time.Duration    `yaml:"shardUpdateCooldown"`
	RateLimit               int              `yaml:"rateLimit"`
	BurstLimit              int              `yaml:"burstLimit"`
	AllShardsConsulAddHosts []string         `yaml:"specialShardsConsulAddHosts"`
}

type Shard struct {
	Shards int `yaml:"shards"`
}

var Config = ConfigType{
	ShardedIngress:      map[string]Shard{"vpn": {Shards: 1}},
	ShardedHTTPProxy:    map[string]Shard{"vpn": {Shards: 1}},
	TerminationPeriod:   time.Minute * 10,
	ShardUpdateCooldown: time.Minute * 1,
	RateLimit:           10,
	BurstLimit:          100,
	AllShardsConsulAddHosts: []string{
		"query.consul",
	},
}

func LoadConfig(path string) (*ConfigType, error) {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	err = yaml.Unmarshal(bytes, &Config)
	if err != nil {
		return nil, err
	}

	return &Config, nil
}
