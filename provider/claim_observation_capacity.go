// Reporting capacity never consumes the space needed by original signed
// custody. Prior optional observations are retained only while they fit.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
)

func marshalClaimQueue(queue *ClaimQueue) ([]byte, error) {
	raw, err := json.MarshalIndent(queue, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// Only the pressure path rereads prior actual bytes. Operational fields are
// copied unchanged; no queue entry, signature, counter or completed outcome is
// removed to fit a monitoring field.
func (self *claimQueueStore) observationCapacity(queue *ClaimQueue, raw []byte) (*ClaimQueue, []byte, uint64, error) {
	if len(raw) <= maximumClaimQueueBytes {
		return queue, raw, 0, nil
	}
	priorRaw, present, err := self.head.Read()
	if err != nil {
		return nil, nil, 0, err
	}
	prior := ClaimQueue{}
	if present {
		if err := json.Unmarshal(priorRaw, &prior); err != nil {
			return nil, nil, 0, err
		}
	}
	copy := *queue
	copy.Entries = make(map[string]*ClaimQueueEntry, len(queue.Entries))
	omitted := uint64(0)
	for epoch, entry := range queue.Entries {
		if entry == nil {
			return nil, nil, 0, fmt.Errorf("claim queue entry %s is absent", epoch)
		}
		record := *entry
		record.PublicObservation = nil
		if retained := prior.Entries[epoch]; retained != nil {
			record.PublicObservation = retained.PublicObservation
		}
		old, _ := json.Marshal(record.PublicObservation)
		next, _ := json.Marshal(entry.PublicObservation)
		if !bytes.Equal(old, next) {
			omitted++
		}
		copy.Entries[epoch] = &record
	}
	fallback, err := marshalClaimQueue(&copy)
	if err != nil {
		return nil, nil, 0, err
	}
	if len(fallback) > maximumClaimQueueBytes {
		// Even old optional metadata must yield to operational history growth.
		// This removes no queue record or original signed/receipt field.
		omitted = 0
		for epoch, entry := range copy.Entries {
			if entry.PublicObservation != nil || queue.Entries[epoch].PublicObservation != nil {
				omitted++
			}
			entry.PublicObservation = nil
		}
		fallback, err = marshalClaimQueue(&copy)
		if err != nil {
			return nil, nil, 0, err
		}
		if len(fallback) > maximumClaimQueueBytes {
			return nil, nil, 0, errClaimQueueCapacity
		}
	}
	return &copy, fallback, omitted, nil
}

// This runs only after the exact fallback bytes have been durably acknowledged.
func adoptClaimObservationCapacity(queue, committed *ClaimQueue) {
	if queue == committed {
		return
	}
	for epoch, entry := range queue.Entries {
		entry.PublicObservation = committed.Entries[epoch].PublicObservation
	}
}
