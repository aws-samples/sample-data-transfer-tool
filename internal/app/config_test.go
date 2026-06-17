package app

import "testing"

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestFromEnv_Defaults(t *testing.T) {
	c, err := FromEnv(envMap(map[string]string{
		"AWS_REGION": "eu-south-2",
		"QUEUE_URL":  "https://sqs/q",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Workers != 16 || c.Receivers != 2 {
		t.Errorf("默认并发错: workers=%d receivers=%d", c.Workers, c.Receivers)
	}
	if c.StatusTable != "transfer-message-status-eu-south-2" {
		t.Errorf("默认表名错: %s", c.StatusTable)
	}
	if c.RCDAddr != "127.0.0.1:5572" {
		t.Errorf("默认 rcd addr 错: %s", c.RCDAddr)
	}
}

func TestFromEnv_MissingRequired(t *testing.T) {
	if _, err := FromEnv(envMap(map[string]string{"QUEUE_URL": "x"})); err == nil {
		t.Error("缺 AWS_REGION 应报错")
	}
	if _, err := FromEnv(envMap(map[string]string{"AWS_REGION": "x"})); err == nil {
		t.Error("缺 QUEUE_URL 应报错")
	}
}

// 防双写不变量：timeout > 0.7×visibility 必须 fail-fast。
func TestTimeoutInvariant(t *testing.T) {
	base := map[string]string{"AWS_REGION": "eu-south-2", "QUEUE_URL": "q"}

	// 恰好 0.7×（43200×0.7=30240）应通过
	base["QUEUE_VISIBILITY_TIMEOUT"] = "43200"
	base["RCLONE_TIMEOUT_SECONDS"] = "30240"
	if _, err := FromEnv(envMap(base)); err != nil {
		t.Errorf("恰好 0.7× 应通过，got %v", err)
	}

	// 超 0.7×（30241）应拒绝
	base["RCLONE_TIMEOUT_SECONDS"] = "30241"
	if _, err := FromEnv(envMap(base)); err == nil {
		t.Error("timeout 超 0.7×visibility 应 fail-fast")
	}

	// visibility=0 应拒绝
	base["QUEUE_VISIBILITY_TIMEOUT"] = "0"
	base["RCLONE_TIMEOUT_SECONDS"] = "1"
	if _, err := FromEnv(envMap(base)); err == nil {
		t.Error("visibility=0 应拒绝")
	}
}
