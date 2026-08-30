package stub

import "testing"

func TestLoadConfig(t *testing.T) {
	t.Setenv("IRS_STUB_ADDR", "127.0.0.1:9000")
	t.Setenv("IRS_STUB_BEARER_TOKEN", "test-token")
	t.Setenv("IRS_STUB_FAIL_BEFORE_RECORD_PERCENT", "7.5")
	t.Setenv("IRS_STUB_FAIL_AFTER_RECORD_PERCENT", "5")
	t.Setenv("IRS_STUB_NEVER_ACK_PERCENT", "2")

	config, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if config.Addr != "127.0.0.1:9000" || config.BearerToken != "test-token" {
		t.Fatalf("LoadConfig() address/token = %#v", config)
	}
	if config.FailBeforeRecordPercent != 7.5 || config.FailAfterRecordPercent != 5 || config.NeverAckPercent != 2 {
		t.Fatalf("LoadConfig() percentages = %#v", config)
	}
}

func TestLoadConfigRejectsInvalidPercentages(t *testing.T) {
	tests := []struct {
		name     string
		before   string
		after    string
		neverAck string
	}{
		{name: "negative", before: "-1", after: "0", neverAck: "0"},
		{name: "over one hundred", before: "101", after: "0", neverAck: "0"},
		{name: "not a number", before: "nope", after: "0", neverAck: "0"},
		{name: "sum over one hundred", before: "60", after: "41", neverAck: "0"},
		{name: "never ack over one hundred", before: "0", after: "0", neverAck: "100.1"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("IRS_STUB_FAIL_BEFORE_RECORD_PERCENT", test.before)
			t.Setenv("IRS_STUB_FAIL_AFTER_RECORD_PERCENT", test.after)
			t.Setenv("IRS_STUB_NEVER_ACK_PERCENT", test.neverAck)
			if _, err := LoadConfig(); err == nil {
				t.Fatal("LoadConfig() error = nil, want validation error")
			}
		})
	}
}

func TestLoadConfigAllowsAlwaysFail(t *testing.T) {
	t.Setenv("IRS_STUB_FAIL_BEFORE_RECORD_PERCENT", "0")
	t.Setenv("IRS_STUB_FAIL_AFTER_RECORD_PERCENT", "100")
	t.Setenv("IRS_STUB_NEVER_ACK_PERCENT", "100")

	config, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if config.FailAfterRecordPercent != 100 || config.NeverAckPercent != 100 {
		t.Fatalf("LoadConfig() percentages = %#v", config)
	}
}
