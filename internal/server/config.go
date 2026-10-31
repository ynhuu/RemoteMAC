package server

import (
	"fmt"
	"os"
	"strings"
)

type Config struct {
	Addr                    string
	Token                   string
	Version                 string
	EnableTestNetworkShaper bool
}

func LoadConfig() (Config, error) {
	token := os.Getenv("REMOTEMAC_TOKEN")
	if strings.TrimSpace(token) == "" {
		return Config{}, fmt.Errorf("请通过环境变量 REMOTEMAC_TOKEN 设置访问口令")
	}
	return Config{
		Addr:                    "0.0.0.0:8081",
		Token:                   token,
		EnableTestNetworkShaper: os.Getenv("REMOTEMAC_ENABLE_TEST_NETWORK_SHAPER") == "1",
	}, nil
}
