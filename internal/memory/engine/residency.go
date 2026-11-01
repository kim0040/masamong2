package engine

import (
	"runtime"
	"time"
)

// Stage-zero residency measurement (ADR-18 §3.8, §6). Nothing here changes what a search does: the
// estimate is arithmetic on counts, and the rebuild samples are a few runtime.ReadMemStats calls made
// by a rebuild, never by a search.

// Room cost model. The numbers start from ADR-18 §5 E4 (a Windows run with 1024-dimension message
// vectors: about 3 MiB for the smallest room, about 1.1 to 1.6 KB per message for large ones) and are
// the only place they live. Message vectors are now 512 wide, so the vector part is computed from the
// width; C-4 replaces these constants with measurements. The result is an estimate, never a measured
// size, and it carries E4's ±30%.
const (
	// EstimateRoomFixedBytes is what a room costs before its first message: lookups, the first leaves and
	// the share of Go's own heap overhead the smallest measured rooms showed.
	EstimateRoomFixedBytes = 3 << 20
	// EstimateBytesPerMessage is a message's cost without its message vector: the text and its leaf, the
	// leaf and summary vectors (one leaf per about ten messages) and the lookups.
	EstimateBytesPerMessage = 700
	// EstimateMessageVectorPermille is how many messages in a thousand hold a message vector. E4's rooms
	// had 33% to 67%; closed leaves only are embedded, and short or empty messages are skipped.
	EstimateMessageVectorPermille = 550
	// estimateVectorRowOverhead is a message vector row's slice header and scale beside its int8 data.
	estimateVectorRowOverhead = 28
	// estimateDefaultMessageDims stands in for a message width that is not known yet.
	estimateDefaultMessageDims = 512
)

// EstimateRoomBytesAt estimates the memory a room with the given number of messages would hold once
// resident: a fixed cost plus a cost per message. messageDims is the message-vector width, 0 when
// message vectors are off. It does not look at the room, so it also prices a room that is not loaded.
func EstimateRoomBytesAt(messages, messageDims int) int64 {
	if messages <= 0 {
		return 0
	}
	perMessage := int64(EstimateBytesPerMessage)
	if messageDims > 0 {
		perMessage += (int64(messageDims) + estimateVectorRowOverhead) * EstimateMessageVectorPermille / 1000
	}
	return EstimateRoomFixedBytes + perMessage*int64(messages)
}

// EstimateRoomBytes is EstimateRoomBytesAt with this engine's message-vector setting and width: the
// width of the loaded index when there is one, else the configured one, else a 512 stand-in.
func (e *Engine) EstimateRoomBytes(messages int) int64 {
	dims := 0
	if e.Settings.MessageVectors {
		dims = e.MessageVectorDims()
		if ix := e.Index(); ix != nil && ix.MsgVecs.Dims > 0 {
			dims = ix.MsgVecs.Dims
		}
		if dims == 0 {
			dims = estimateDefaultMessageDims
		}
	}
	return EstimateRoomBytesAt(messages, dims)
}

// Stages at which a rebuild takes a memory sample. They are boundaries between steps, not instants of
// the rebuild's highest memory.
const (
	StageRead           = "read"            // the database rows and leaf vectors are in; the full-text indexes are not built yet
	StageBuild          = "build"           // search.Build is done: lookups and both full-text databases exist
	StageMessageVectors = "message_vectors" // message vectors are loaded and attached (only when they are on)
	StagePublish        = "publish"         // just before the new index replaces the old one
)

// MeasurementBasis says what ReloadMeasurement.MaxHeapAlloc is, for diagnostics that print it.
const MeasurementBasis = "stage_boundary_samples"

// Outcomes of a measured rebuild.
const (
	ReloadPublished = "published"
	ReloadDiscarded = "discarded" // the engine was replaced while building; the result was closed
	ReloadFailed    = "failed"
)

// ReloadSample is one runtime.MemStats reading at a stage boundary.
type ReloadSample struct {
	Stage     string
	Since     time.Duration // from the rebuild's start
	HeapAlloc uint64        // live objects plus garbage not collected yet
	HeapInuse uint64
	NumGC     uint32
}

// ReloadMeasurement is what the latest rebuild sampled. A sample is taken only at the stage boundaries
// above, so MaxHeapAlloc is the largest of those readings: the rebuild can have been higher between
// them, and garbage counts until the next collection. It is not the true peak. A rebuild that failed
// keeps the samples it reached, so the stage it stopped after is known.
type ReloadMeasurement struct {
	StartedAt time.Time
	Outcome   string
	Samples   []ReloadSample
}

// MaxHeapAlloc is the largest HeapAlloc of the stage-boundary samples (0 without samples).
func (m ReloadMeasurement) MaxHeapAlloc() uint64 {
	var max uint64
	for _, s := range m.Samples {
		if s.HeapAlloc > max {
			max = s.HeapAlloc
		}
	}
	return max
}

// LastStage is the stage of the last sample ("" without samples). After a failure it is the last stage
// the rebuild finished.
func (m ReloadMeasurement) LastStage() string {
	if len(m.Samples) == 0 {
		return ""
	}
	return m.Samples[len(m.Samples)-1].Stage
}

// reloadRecorder takes the samples of one rebuild. A nil recorder takes none.
type reloadRecorder struct {
	started time.Time
	samples []ReloadSample
}

func (r *reloadRecorder) sample(stage string) {
	if r == nil {
		return
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms) // stops the world briefly; four times per rebuild, never on a search
	r.samples = append(r.samples, ReloadSample{Stage: stage, Since: time.Since(r.started), HeapAlloc: ms.HeapAlloc, HeapInuse: ms.HeapInuse, NumGC: ms.NumGC})
}

// ResidencyMeasurement returns what the latest rebuild sampled; ok is false before the first rebuild.
func (e *Engine) ResidencyMeasurement() (m ReloadMeasurement, ok bool) {
	p := e.lastMeasure.Load()
	if p == nil {
		return ReloadMeasurement{}, false
	}
	m = *p
	m.Samples = append([]ReloadSample(nil), m.Samples...)
	return m, true
}

// finishMeasurement records the rebuild's outcome and reports it. A failure before the first stage is
// recorded too, with no samples: "failed before reading finished" is itself the answer.
func (e *Engine) finishMeasurement(r *reloadRecorder, outcome string) {
	m := &ReloadMeasurement{StartedAt: r.started, Outcome: outcome, Samples: r.samples}
	e.lastMeasure.Store(m)
	if f := e.OnReloadMeasurement; f != nil {
		cp := *m
		cp.Samples = append([]ReloadSample(nil), m.Samples...)
		f(cp)
	}
}
