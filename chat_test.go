package main

import "testing"

func TestParseResponseFramePreservesMultipleToolDeltas(t *testing.T) {
	var firstTool pw
	firstTool.str(1, "functions.read_file:0")
	firstTool.str(2, "read_file")
	firstTool.str(3, `{"path":"a"}`)

	var secondTool pw
	secondTool.str(1, "functions.list_files:1")
	secondTool.str(2, "list_files")
	secondTool.str(3, `{"path":"b"}`)

	var responseFrame pw
	responseFrame.msg(6, firstTool.bytes())
	responseFrame.msg(6, secondTool.bytes())

	delta := parseResponseFrame(responseFrame.bytes())
	if len(delta.tools) != 2 {
		t.Fatalf("parsed %d tool deltas, want 2", len(delta.tools))
	}
	if delta.tools[0].id != "functions.read_file:0" || delta.tools[0].args != `{"path":"a"}` {
		t.Fatalf("unexpected first tool delta: %+v", delta.tools[0])
	}
	if delta.tools[1].id != "functions.list_files:1" || delta.tools[1].args != `{"path":"b"}` {
		t.Fatalf("unexpected second tool delta: %+v", delta.tools[1])
	}
}

func TestAccumulateToolDeltasRoutesFragmentsByID(t *testing.T) {
	var tools []*toolAcc
	toolIndexes := map[string]int{}
	activeToolIndex := -1

	deltaGroups := [][]toolDelta{
		{{id: "tool-a", name: "first", args: "{"}, {id: "tool-b", name: "second", args: "["}},
		{{id: "tool-a", args: "}"}, {args: "!"}},
	}
	for _, deltas := range deltaGroups {
		tools, activeToolIndex = accumulateToolDeltas(tools, toolIndexes, activeToolIndex, deltas)
	}

	if len(tools) != 2 {
		t.Fatalf("accumulated %d tools, want 2", len(tools))
	}
	if got := tools[0].args.String(); got != "{}!" {
		t.Fatalf("tool-a arguments = %q, want %q", got, "{}!")
	}
	if got := tools[1].args.String(); got != "[" {
		t.Fatalf("tool-b arguments = %q, want %q", got, "[")
	}
}
