package main

// 跨语言对拍测试：用同一批用例分别跑 JS 原版（src/index.js，经 go/testdata/compat_js_runner.cjs
// 的 Node runner 输出）与本 Go 实现，做语义级对比（JSON 解析后 deep-equal，解析失败则字节等价）。
//
// 运行顺序：
//   node go/testdata/compat_js_runner.cjs   # 生成 /tmp/compat/js_out.json
//   go test -run TestCompatWithNode ./go
//
// js_out.json 缺失时跳过，不影响常规 go test。

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestCompatWithNode(t *testing.T) {
	jsOutPath := "/tmp/compat/js_out.json"
	if _, err := os.Stat(jsOutPath); err != nil {
		t.Skip("js_out.json not present; run `node /tmp/compat/run_js.js` first")
	}

	var cases []struct {
		ID    string `json:"id"`
		Kind  string `json:"kind"`
		Input string `json:"input"`
	}
	loadJSON(t, "testdata/compat_cases.json", &cases)

	var jsResults []struct {
		ID     string          `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	loadJSON(t, jsOutPath, &jsResults)
	jsByID := make(map[string]json.RawMessage, len(jsResults))
	for _, j := range jsResults {
		jsByID[j.ID] = j.Result
	}

	deltas := 0
	for _, c := range cases {
		jsRaw, ok := jsByID[c.ID]
		if !ok {
			t.Fatalf("case %s missing from js_out.json", c.ID)
		}
		var jsResult map[string]any
		if err := json.Unmarshal(jsRaw, &jsResult); err != nil {
			t.Fatalf("js result for %s not an object: %v", c.ID, err)
		}

		var goResult map[string]any
		switch c.Kind {
		case "delta":
			out, changed := rewriteDelta([]byte(c.Input))
			goResult = map[string]any{"changed": changed, "out": string(out)}
		case "frame":
			out := processFrame([]byte(c.Input))
			if out == nil {
				goResult = map[string]any{"out": nil}
			} else {
				goResult = map[string]any{"out": string(out)}
			}
		case "msg":
			out, ok := rewriteNonStream([]byte(c.Input))
			goResult = map[string]any{"ok": ok, "out": string(out)}
		}

		// out 字段做语义对比（JSON 解析后 deep-equal；解析失败则字节相等）
		jsOut, goOut := jsResult["out"], goResult["out"]
		if !semanticOutEqual(jsOut, goOut) {
			t.Errorf("case %s (%s) mismatch:\n  js : %s\n  go : %s",
				c.ID, c.Kind, dump(jsResult), dump(goResult))
		}
		// changed/ok 布尔位必须完全一致
		if jsResult["changed"] != goResult["changed"] {
			t.Errorf("case %s changed differs: js=%v go=%v", c.ID, jsResult["changed"], goResult["changed"])
		}
		if jsResult["ok"] != goResult["ok"] {
			t.Errorf("case %s ok differs: js=%v go=%v", c.ID, jsResult["ok"], goResult["ok"])
		}
		deltas++
	}
	if deltas == 0 {
		t.Fatal("no cases compared")
	}
	t.Logf("compared %d cases", deltas)
}

func loadJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

// semanticOutEqual：两侧都是 "JSON 文本" 时解析后 deep-equal；否则做 SSE 感知的行级比较
// （data 行的值按 JSON 语义比较、忽略字段序，其余行字节相等）。
func semanticOutEqual(a, b any) bool {
	as, aok := a.(string)
	bs, bok := b.(string)
	if !aok || !bok {
		return reflect.DeepEqual(a, b)
	}
	var ja, jb any
	ea := json.Unmarshal([]byte(as), &ja)
	eb := json.Unmarshal([]byte(bs), &jb)
	if ea == nil && eb == nil {
		return reflect.DeepEqual(ja, jb)
	}
	return sseFrameEqual(as, bs)
}

// sseFrameEqual：比较两个 SSE 帧文本，非 data 行字节相等、data 行 JSON 语义相等。
func sseFrameEqual(x, y string) bool {
	xs, ys := strings.Split(x, "\n"), strings.Split(y, "\n")
	if len(xs) != len(ys) {
		return false
	}
	for i := range xs {
		if !sseLineEqual(xs[i], ys[i]) {
			return false
		}
	}
	return true
}

func sseLineEqual(a, b string) bool {
	if strings.HasPrefix(a, "data:") && strings.HasPrefix(b, "data:") {
		va := strings.TrimSpace(a[len("data:"):])
		vb := strings.TrimSpace(b[len("data:"):])
		var ja, jb any
		ea := json.Unmarshal([]byte(va), &ja)
		eb := json.Unmarshal([]byte(vb), &jb)
		if ea == nil && eb == nil {
			return reflect.DeepEqual(ja, jb)
		}
	}
	return a == b
}

func dump(v map[string]any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}
