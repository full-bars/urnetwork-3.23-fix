package urnettools

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ExtenderGossipEvent mirrors gossip.Event from the provider's flight recorder.
type ExtenderGossipEvent struct {
	Time      time.Time `json:"time"`
	Kind      string    `json:"kind"`
	Peer      string    `json:"peer,omitempty"`
	Direction string    `json:"direction,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Size      int       `json:"size,omitempty"`
}

type extenderEventsFilter struct {
	Since      time.Duration
	Kinds      map[string]bool
	JSONOutput bool
	Target     Target
}

var (
	socketExtenderEventsFn = queryExtenderEventsFromSocket
	extenderEventsNowFn    = time.Now
)

var validExtenderEventKinds = map[string]bool{
	"peer":    true,
	"reject":  true,
	"state":   true,
	"dial":    true,
	"cap":     true,
	"publish": true,
}

func isExtenderEventsArgs(args []string) bool {
	_, rest, err := parseTargetFlagsLenient(args)
	if err != nil {
		for _, a := range args {
			if strings.EqualFold(strings.TrimSpace(a), "events") {
				return true
			}
		}
		return false
	}
	for i := 0; i < len(rest); i++ {
		tok := rest[i]
		switch {
		case tok == "--json":
			continue
		case (tok == "--since" || tok == "--kind") && i+1 < len(rest):
			i++
			continue
		case strings.HasPrefix(tok, "--since=") || strings.HasPrefix(tok, "--kind="):
			continue
		default:
			return strings.EqualFold(strings.TrimSpace(tok), "events")
		}
	}
	return false
}

func parseExtenderEventsArgs(args []string) (extenderEventsFilter, error) {
	var filter extenderEventsFilter
	var filtered []string
	for i := 0; i < len(args); i++ {
		tok := args[i]
		switch {
		case tok == "--json":
			filter.JSONOutput = true
		case tok == "--since":
			if i+1 >= len(args) {
				return filter, fmt.Errorf("%s (missing value for --since)", extenderEventsUsage)
			}
			i++
			d, err := parseExtenderEventsSince(args[i])
			if err != nil {
				return filter, err
			}
			filter.Since = d
		case strings.HasPrefix(tok, "--since="):
			d, err := parseExtenderEventsSince(strings.TrimPrefix(tok, "--since="))
			if err != nil {
				return filter, err
			}
			filter.Since = d
		case tok == "--kind":
			if i+1 >= len(args) {
				return filter, fmt.Errorf("%s (missing value for --kind)", extenderEventsUsage)
			}
			i++
			if err := addExtenderEventKinds(&filter, args[i]); err != nil {
				return filter, err
			}
		case strings.HasPrefix(tok, "--kind="):
			if err := addExtenderEventKinds(&filter, strings.TrimPrefix(tok, "--kind=")); err != nil {
				return filter, err
			}
		default:
			filtered = append(filtered, tok)
		}
	}
	target, rest, err := parseTargetFlags(filtered)
	if err != nil {
		return filter, err
	}
	filter.Target = target
	seenEvents := false
	for _, tok := range rest {
		if !seenEvents && strings.EqualFold(strings.TrimSpace(tok), "events") {
			seenEvents = true
			continue
		}
		return filter, fmt.Errorf("%s (unexpected %q)", extenderEventsUsage, tok)
	}
	if !seenEvents {
		return filter, fmt.Errorf("%s", extenderEventsUsage)
	}
	return filter, nil
}

func parseExtenderEventsSince(raw string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s (invalid --since duration %q; want a positive duration such as 1h or 30m)", extenderEventsUsage, raw)
	}
	return d, nil
}

func addExtenderEventKinds(filter *extenderEventsFilter, raw string) error {
	if filter.Kinds == nil {
		filter.Kinds = make(map[string]bool)
	}
	parts := strings.Split(raw, ",")
	added := 0
	for _, part := range parts {
		k := strings.ToLower(strings.TrimSpace(part))
		if k == "" {
			continue
		}
		if !validExtenderEventKinds[k] {
			return fmt.Errorf("%s (invalid kind %q; valid kinds are peer, reject, state, dial, cap, publish)", extenderEventsUsage, part)
		}
		filter.Kinds[k] = true
		added++
	}
	if added == 0 {
		return fmt.Errorf("%s (empty --kind)", extenderEventsUsage)
	}
	return nil
}

func queryExtenderEventsFromSocket(p Provider) ([]ExtenderGossipEvent, error) {
	if p.StateDir == "" {
		return nil, errors.New("no state directory")
	}
	sockPath := filepath.Join(p.StateDir, "provider.sock")
	resp, err := sendSocketRequest(sockPath, controlRequest{Cmd: "extender_events"})
	if err != nil {
		return nil, extenderSocketError(sockPath, err)
	}
	if !resp.OK {
		if resp.Error != "" {
			return nil, errors.New(resp.Error)
		}
		return nil, errors.New("this provider build does not report extender events (update it)")
	}
	return resp.GossipEvents, nil
}

func filterExtenderGossipEvents(events []ExtenderGossipEvent, filter extenderEventsFilter, now time.Time) []ExtenderGossipEvent {
	out := make([]ExtenderGossipEvent, 0, len(events))
	var cutoff time.Time
	if filter.Since > 0 {
		cutoff = now.Add(-filter.Since)
	}
	for _, ev := range events {
		if !cutoff.IsZero() && ev.Time.Before(cutoff) {
			continue
		}
		if len(filter.Kinds) > 0 && !filter.Kinds[strings.ToLower(ev.Kind)] {
			continue
		}
		out = append(out, ev)
	}
	return out
}

func formatExtenderGossipEvents(w io.Writer, events []ExtenderGossipEvent, filter extenderEventsFilter, now time.Time) error {
	filtered := filterExtenderGossipEvents(events, filter, now)
	if filter.JSONOutput {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(filtered)
	}
	if len(filtered) == 0 {
		fmt.Fprintln(w, "No gossip events.")
		return nil
	}
	for _, ev := range filtered {
		var b strings.Builder
		fmt.Fprintf(&b, "%s  %-7s", ev.Time.UTC().Format(time.RFC3339), ev.Kind)
		if ev.Peer != "" {
			fmt.Fprintf(&b, "  peer=%s", ev.Peer)
		}
		if ev.Direction != "" {
			fmt.Fprintf(&b, "  dir=%s", ev.Direction)
		}
		if ev.Reason != "" {
			fmt.Fprintf(&b, "  reason=%s", ev.Reason)
		}
		if ev.Size > 0 {
			fmt.Fprintf(&b, "  size=%d", ev.Size)
		}
		fmt.Fprintln(w, b.String())
	}
	return nil
}

func cmdExtenderEvents(args []string) error {
	filter, err := parseExtenderEventsArgs(args)
	if err != nil {
		return err
	}
	p, err := selectTarget(Discover(), filter.Target)
	if err != nil {
		return err
	}
	if p.StateDir == "" {
		return fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
	}
	if elevated, err := maybeElevateForCrossUser("extender", p, args, false, false); elevated {
		return err
	}
	events, err := socketExtenderEventsFn(p)
	if err != nil {
		return err
	}
	return formatExtenderGossipEvents(os.Stdout, events, filter, extenderEventsNowFn())
}
