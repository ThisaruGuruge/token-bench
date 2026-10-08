package streamlog

import (
	"reflect"
	"strings"
	"testing"
)

const sampleLog = `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"hi"}]}}
{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"bal discover ballerinax/twilio client && bal search twilio"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}
{"type":"assistant","message":{"id":"m2","content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"cd app && BAL_X=1 /usr/local/bin/bal build 2>&1 | tail -5"}}]}}
{"type":"assistant","message":{"id":"m3","content":[{"type":"tool_use","id":"t3","name":"WebSearch","input":{"query":"ballerina twilio"}},{"type":"tool_use","id":"t4","name":"WebFetch","input":{"url":"https://example.com"}}]}}
{"type":"assistant","parent_tool_use_id":"t5","message":{"id":"m4","content":[{"type":"tool_use","id":"t6","name":"Read","input":{"file_path":"/Users/x/.ballerina/repositories/central.ballerina.io/bala/a.bal"}}]}}
{"type":"assistant","message":{"id":"m5","content":[{"type":"tool_use","id":"t5","name":"Task","input":{"description":"x"}},{"type":"tool_use","id":"t7","name":"mcp__library__search","input":{}},{"type":"tool_use","id":"t8","name":"Grep","input":{"pattern":"foo","path":"/home/u/.ballerina/repositories"}},{"type":"tool_use","id":"t9","name":"Read","input":{"file_path":"main.bal"}}]}}
not json
{"type":"result","subtype":"success","total_cost_usd":0.5,"result":"done"}
{"type":"result","subtype":"success","total_cost_usd":0.75}
`

func TestAnalyze(t *testing.T) {
	summary, err := Analyze(strings.NewReader(sampleLog))
	if err != nil {
		t.Fatal(err)
	}
	if summary.AssistantTurns != 5 || summary.SubagentTurns != 1 {
		t.Errorf("turns = %d (subagent %d), want 5 (1)", summary.AssistantTurns, summary.SubagentTurns)
	}
	if summary.ToolCalls != 9 {
		t.Errorf("tool calls = %d, want 9", summary.ToolCalls)
	}
	wantTools := map[string]int{"Bash": 2, "WebSearch": 1, "WebFetch": 1, "Read": 2, "Task": 1, "mcp__library__search": 1, "Grep": 1}
	if !reflect.DeepEqual(summary.Tools, wantTools) {
		t.Errorf("tools = %v, want %v", summary.Tools, wantTools)
	}
	wantFamilies := map[string]int{
		FamilyBalDiscover: 1, FamilyBalSearch: 1, FamilyBalBuild: 1, FamilyWebSearch: 1, FamilyWebFetch: 1,
		FamilyAgent: 1, FamilyMCP: 1, FamilyBalRepoRead: 2,
	}
	if !reflect.DeepEqual(summary.Families, wantFamilies) {
		t.Errorf("families = %v, want %v", summary.Families, wantFamilies)
	}
	if !reflect.DeepEqual(summary.ResultCostsUSD, []float64{0.5, 0.75}) {
		t.Errorf("costs = %v", summary.ResultCostsUSD)
	}
	if summary.MalformedLines != 1 {
		t.Errorf("malformed = %d, want 1", summary.MalformedLines)
	}
}

func TestAnalyzeEmptyLog(t *testing.T) {
	summary, err := Analyze(strings.NewReader(""))
	if err != nil || summary.ToolCalls != 0 || summary.Tools == nil || summary.Families == nil {
		t.Fatalf("unexpected summary %+v, err %v", summary, err)
	}
}

func TestBashFamilies(t *testing.T) {
	tests := map[string][]string{
		"bal discover ballerinax/x":              {FamilyBalDiscover},
		"bal --help":                             {FamilyBalOther},
		"bal new foo":                            {FamilyBalOther},
		"bal pull a/b; bal run":                  {FamilyBalPull, FamilyBalRun},
		"bal search x | head && bal search y":    {FamilyBalSearch},
		"cd x && bal build":                      {FamilyBalBuild},
		"ls; cat file":                           nil,
		"echo bal discover":                      nil,
		"FOO=1 bal run --offline":                {FamilyBalRun},
		"bal -v build":                           {FamilyBalBuild},
		"(cd a && bal run) 2>&1":                 {FamilyBalRun},
		"bal\ndiscover":                          {FamilyBalOther},
		"bash -c 'bal discover ballerinax/x'":    {FamilyBalDiscover},
		`sh -lc "cd a && bal build"`:             {FamilyBalBuild},
		"timeout 120 bal discover a/b":           {FamilyBalDiscover},
		"timeout --signal=KILL 60s bal pull a/b": {FamilyBalPull},
		"env -i FOO=1 bal search x":              {FamilyBalSearch},
		"time bal build":                         {FamilyBalBuild},
		"bash script.sh":                         nil,
		"echo 'bal discover'":                    nil,
		"'bal' 'discover' a/b":                   {FamilyBalDiscover},
	}
	for command, want := range tests {
		if got := bashFamilies(command); !reflect.DeepEqual(got, want) {
			t.Errorf("bashFamilies(%q) = %v, want %v", command, got, want)
		}
	}
}

func TestAnalyzeFileMissing(t *testing.T) {
	if _, err := AnalyzeFile("/nonexistent/claude-stream.jsonl"); err == nil {
		t.Fatal("expected error")
	}
}

const usageLog = `{"type":"assistant","message":{"id":"a1","model":"claude-sonnet-5-5","content":[{"type":"text"}],"usage":{"input_tokens":5,"output_tokens":1,"cache_read_input_tokens":100,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":30}}}}
{"type":"assistant","message":{"id":"a1","model":"claude-sonnet-5-5","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}],"usage":{"input_tokens":5,"output_tokens":40,"cache_read_input_tokens":100,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":30}}}}
{"type":"assistant","message":{"id":"a2","model":"claude-sonnet-5-5","content":[],"usage":{"input_tokens":2,"output_tokens":10,"cache_read_input_tokens":130,"cache_creation_input_tokens":12,"cache_creation":{"ephemeral_5m_input_tokens":2,"ephemeral_1h_input_tokens":10}}}}
{"type":"assistant","parent_tool_use_id":"t1","message":{"id":"s1","model":"claude-haiku-5-5","content":[],"usage":{"input_tokens":7,"output_tokens":3,"cache_read_input_tokens":0,"cache_creation_input_tokens":9}}}
{"type":"assistant","message":{"id":"x1","model":"<synthetic>","content":[],"usage":{"input_tokens":0,"output_tokens":0}}}
`

func TestAnalyzeModelUsage(t *testing.T) {
	summary, err := Analyze(strings.NewReader(usageLog))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]ModelUsage{
		// a1 counted once (largest snapshot) plus a2.
		"claude-sonnet-5-5": {Messages: 2, Input: 7, Output: 50, CacheRead: 230, CacheWrite5m: 2, CacheWrite1h: 40},
		"claude-haiku-5-5":  {Messages: 1, Input: 7, Output: 3, CacheWriteUnsplit: 9},
	}
	if !reflect.DeepEqual(summary.Models, want) {
		t.Errorf("models = %+v, want %+v", summary.Models, want)
	}
}
