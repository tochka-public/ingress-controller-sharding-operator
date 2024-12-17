package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
)

type ConfigType struct {
	ShardedIngress struct {
		Shards map[string]int `mapstructure:"shards"`
	} `mapstructure:"shardedIngress"`
	ShardedHTTPProxy struct {
		Shards map[string]int `mapstructure:"shards"`
		Labels struct {
			RootHTTPProxy string `mapstructure:"rootHTTPProxy"`
		} `mapstructure:"labels"`
		Annotations struct {
			VirtualHosts string `mapstructure:"virtualHosts"`
		} `mapstructure:"annotations"`
	} `mapstructure:"shardedHTTPProxy"`
	RateLimit struct {
		UpdateCooldown struct {
			Object time.Duration `mapstructure:"object"`
			Shard  time.Duration `mapstructure:"shard"`
		} `mapstructure:"updateCooldown"`
		ApiRateLimit  int `mapstructure:"apiRateLimit"`
		ApiBurstLimit int `mapstructure:"apiBurstLimit"`
	} `mapstructure:"rateLimit"`
	General struct {
		DomainSubstring string `mapstructure:"domainSubstring"`
		Annotations     struct {
			MutatingWebhook string `mapstructure:"mutatingWebhook"`
		} `mapstructure:"annotations"`
	} `mapstructure:"general"`
	AdditionalServiceDiscovery struct {
		Labels struct {
			Class   string `mapstructure:"class"`
			AppName string `mapstructure:"appName"`
		} `mapstructure:"labels"`
		Annotations struct {
			Tags          string `mapstructure:"tags"`
			Unregistering string `mapstructure:"unregistering"`
		}
	} `mapstructure:"additionalServiceDiscovery"`
	AllShardsPlacement struct {
		Annotations struct {
			Enabled string `mapstructure:"enabled"`
		} `mapstructure:"annotations"`
		ShardBaseDomains []string `mapstructure:"shardBaseDomains"`
	} `mapstructure:"allShardsPlacement"`
}

func LoadConfig(path string, runShardedIngress, runShardedHTTPProxy bool) (*ConfigType, error) {
	viper.SetOptions(viper.ExperimentalBindStruct())
	viper.SetConfigType("yaml")
	viper.SetDefault("rateLimit.objectUpdateCooldown", 30*time.Second)
	viper.SetDefault("rateLimit.shardUpdateCooldown", 10*time.Second)
	viper.SetDefault("rateLimit.apiRateLimit", 10)
	viper.SetDefault("rateLimit.apiBurstLimit", 100)

	viper.SetDefault("shardedHTTPProxy.labels.rootHTTPProxy", "k8s.tochka.com/base-proxy")
	viper.SetDefault("shardedHTTPProxy.annotations.virtualHosts", "k8s.tochka.com/virtual-hosts")

	viper.SetDefault("additionalServiceDiscovery.labels.class", "k8s.tochka.com/ingress-class")
	viper.SetDefault("additionalServiceDiscovery.labels.appName", "k8s.tochka.com/app-name")
	viper.SetDefault("additionalServiceDiscovery.annotations.tags", "k8s.tochka.com/service-discovery-tags")
	viper.SetDefault("additionalServiceDiscovery.annotations.unregistering", "k8s.tochka.com/marked-for-deletion")

	viper.SetDefault("allShardsPlacement.annotations.enabled", "k8s.tochka.com/use-all-class-shards")

	viper.SetConfigFile(path)

	viper.AutomaticEnv()
	viper.SetEnvPrefix("sharded")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	if err := viper.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("error reading config file: %w", err)
	}

	var config ConfigType
	if err := viper.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("unable to decode into struct: %w", err)
	}

	if shardsEnv := os.Getenv("SHARDED_SHARDEDINGRESS_SHARDS"); shardsEnv != "" {
		var shards map[string]int
		if err := json.Unmarshal([]byte(shardsEnv), &shards); err != nil {
			fmt.Printf("Error parsing JSON for SHARDED_SHARDEDINGRESS_SHARDS: %v\n", err)
		} else {
			config.ShardedIngress.Shards = shards
		}
	}

	if shardsEnv := os.Getenv("SHARDED_SHARDEDHTTPPROXY_SHARDS"); shardsEnv != "" {
		var shards map[string]int
		if err := json.Unmarshal([]byte(shardsEnv), &shards); err != nil {
			fmt.Printf("Error parsing JSON for SHARDED_SHARDEDHTTPPROXY_SHARDS: %v\n", err)
		} else {
			config.ShardedHTTPProxy.Shards = shards
		}
	}

	fmt.Println(config)

	if err := validateConfig(&config, runShardedIngress, runShardedHTTPProxy); err != nil {
		return nil, err
	}

	return &config, nil
}

func validateConfig(conf *ConfigType, runShardedIngress, runShardedHTTPProxy bool) error {
	if runShardedIngress {
		if len(conf.ShardedIngress.Shards) == 0 {
			return fmt.Errorf("shardedIngress.shards must be configured")
		}
	}

	if runShardedHTTPProxy {
		if len(conf.ShardedHTTPProxy.Shards) == 0 {
			return fmt.Errorf("shardedHTTPProxy.shards must be configured")
		}
	}

	if conf.General.DomainSubstring == "" {
		return fmt.Errorf("general.domainSubstring must be configured")
	}

	return nil
}
