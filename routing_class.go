// Ported to urnetwork-3.23-fix from the v2026 engine,
// github.com/full-bars/connect@v0.0.0-20261017075543-c8d8cdf536a8
// (routing_class.go). Unchanged. This is the minimal supporting vocabulary the
// light classifier (routing_classifier_light.go) needs: the class constants,
// FlowClass, the FlowClassifier interface, and the one-branch nil guard. The
// newer generation's scoring/placement consumers of these types
// (scoredPlacementReorder, SetFlowClassifier, the reward path) are NOT ported.
package connect

// TrafficClass is the smart-routing traffic class of a flow. ClassUnknown is
// the zero value on purpose: an un-classified flow (no classifier installed, or
// classification not yet decided) is Unknown, which every scoring path treats
// as "no class preference" so the layer is inert until a classifier is present.
type TrafficClass uint8

const (
	ClassUnknown TrafficClass = iota
	ClassLatency
	ClassStreaming
	ClassBulk
	ClassBrowsing
	ClassBackground
)

func (c TrafficClass) String() string {
	switch c {
	case ClassLatency:
		return "latency"
	case ClassStreaming:
		return "streaming"
	case ClassBulk:
		return "bulk"
	case ClassBrowsing:
		return "browsing"
	case ClassBackground:
		return "background"
	default:
		return "unknown"
	}
}

// FlowClass is a classification result: the class, the owning app (may be ""),
// and a 0-100 confidence.
type FlowClass struct {
	Class      TrafficClass
	AppId      string
	Confidence uint8
}

// FlowClassifier turns a flow into a FlowClass. The nil implementation is the
// legacy path; a real one (nDPI, a later phase) is installed via
// SetFlowClassifier. Implementations must be safe for concurrent calls and must
// not block (see classifyOrUnknown callers on the placement path).
type FlowClassifier interface {
	Classify(ipPath *IpPath, appId string) FlowClass
}

// classifyOrUnknown is the one-branch nil guard the placement path pays when no
// classifier is installed, mirroring the flowOwnerFunc nil check.
func classifyOrUnknown(c FlowClassifier, ipPath *IpPath, appId string) FlowClass {
	if c == nil {
		return FlowClass{Class: ClassUnknown, AppId: appId}
	}
	return c.Classify(ipPath, appId)
}
