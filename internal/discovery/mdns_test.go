package discovery

import "testing"

// TestUnescapeInstance 回归：含中文的设备显示名经 mDNS 实例名读回时被
// miekg/dns 转义为 \DDD（十进制字节值），unescapeInstance 必须还原；
// 否则转义文本会作为设备名入库（ubuntu-latest CI 实测：peers 表名字
// 变成 "\232\138\130\231\130\185B" 而非 "节点B"）。
func TestUnescapeInstance(t *testing.T) {
	// "节点B" 的 UTF-8 字节 E8 8A 82 E7 82 B9 42 的 dns-sd 转义形式
	esc := `\232\138\130\231\130\185B`
	if got := unescapeInstance(esc); got != "节点B" {
		t.Fatalf("unescapeInstance(%q) = %q, 期望 节点B", esc, got)
	}
	if got := unescapeInstance("plain-name"); got != "plain-name" {
		t.Fatalf("无转义的 ASCII 名必须原样: %q", got)
	}
	if got := unescapeInstance(`a\.b`); got != "a.b" {
		t.Fatalf("字面量转义未还原: %q", got)
	}
	if got := unescapeInstance(`tail\`); got != `tail\` {
		t.Fatalf("结尾反斜杠必须原样: %q", got)
	}
	if got := unescapeInstance(`\999`); got != `\999` {
		t.Fatalf("大于 255 的十进制组不是合法字节转义，必须原样: %q", got)
	}
}
