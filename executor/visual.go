package executor

import (
	"context"
	"github.com/gogo/protobuf/proto"
	"github.com/pingcap/tidb/planner/core"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/visual"
)

// VisualExec represents an visual executor.
type VisualExec struct {
	baseExecutor

	visual      *core.Visual
	analyzeExec Executor
	executed    bool
	rows        [][]string
	cursor      int
}

// Open implements the Executor Open interface.
func (e *VisualExec) Open(ctx context.Context) error {
	if e.analyzeExec != nil {
		return e.analyzeExec.Open(ctx)
	}
	return nil
}

// Close implements the Executor Close interface.
func (e *VisualExec) Close() error {
	e.rows = nil
	if e.analyzeExec != nil && !e.executed {
		// Open(), but Next() is not called.
		return e.analyzeExec.Close()
	}
	return nil
}

// Next implements the Executor Next interface.
func (e *VisualExec) Next(ctx context.Context, req *chunk.Chunk) error {
	req.GrowAndReset(e.maxChunkSize)
	if e.cursor > 0 {
		return nil
	}
	res := visual.VisualData{
		Name:            "Select",
		Cost:            100,
		EstRows:         10,
		AstRows:         100,
		AccessIndex:     "",
		AccessTable:     "t",
		AccessPartition: "",
		TimeUs:          10,
		RunAt:           "root",
	}
	resData, err := proto.Marshal(res)
	if err != nil {
		return err
	}
	req.AppendString(0, "hello, visual plan")
	e.cursor = 1
	return nil
}
