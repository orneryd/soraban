package stub

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

const (
	defaultAddr        = "127.0.0.1:8081"
	defaultBearerToken = "local-irs-token"
)

type Config struct {
	Addr                    string
	BearerToken             string
	FailBeforeRecordPercent float64
	FailAfterRecordPercent  float64
	NeverAckPercent         float64
}

func LoadConfig() (Config, error) {
	config := Config{
		Addr:        envOrDefault("IRS_STUB_ADDR", defaultAddr),
		BearerToken: envOrDefault("IRS_STUB_BEARER_TOKEN", defaultBearerToken),
	}

	var err error
	config.FailBeforeRecordPercent, err = percentFromEnv("IRS_STUB_FAIL_BEFORE_RECORD_PERCENT", 7)
	if err != nil {
		return Config{}, err
	}
	config.FailAfterRecordPercent, err = percentFromEnv("IRS_STUB_FAIL_AFTER_RECORD_PERCENT", 5)
	if err != nil {
		return Config{}, err
	}
	config.NeverAckPercent, err = percentFromEnv("IRS_STUB_NEVER_ACK_PERCENT", 0)
	if err != nil {
		return Config{}, err
	}

	if err := config.Validate(); err != nil {
		return Config{}, err
	}

	return config, nil
}

func (config Config) Validate() error {
	if strings.TrimSpace(config.Addr) == "" {
		return fmt.Errorf("IRS_STUB_ADDR must not be empty")
	}
	if config.BearerToken == "" {
		return fmt.Errorf("IRS_STUB_BEARER_TOKEN must not be empty")
	}
	percentages := []struct {
		name  string
		value float64
	}{
		{name: "IRS_STUB_FAIL_BEFORE_RECORD_PERCENT", value: config.FailBeforeRecordPercent},
		{name: "IRS_STUB_FAIL_AFTER_RECORD_PERCENT", value: config.FailAfterRecordPercent},
		{name: "IRS_STUB_NEVER_ACK_PERCENT", value: config.NeverAckPercent},
	}
	for _, percentage := range percentages {
		if math.IsNaN(percentage.value) || math.IsInf(percentage.value, 0) || percentage.value < 0 || percentage.value > 100 {
			return fmt.Errorf("%s must be a number from 0 through 100", percentage.name)
		}
	}
	if config.FailBeforeRecordPercent+config.FailAfterRecordPercent > 100 {
		return fmt.Errorf("before-record and after-record failure percentages must sum to at most 100")
	}
	return nil
}

func envOrDefault(name, fallback string) string {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	return value
}

func percentFromEnv(name string, fallback float64) (float64, error) {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}

	percent, err := strconv.ParseFloat(value, 64)
	if err != nil || percent < 0 || percent > 100 {
		return 0, fmt.Errorf("%s must be a number from 0 through 100", name)
	}
	return percent, nil
}
