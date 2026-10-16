// Miner error inspection has its own finite work budget, independent of the
// network deadline. Unknown, incomplete and typed nil graphs stay terminal.
package main

import "reflect"

const (
	minerReadCauseMaximumDepth = 32
	minerReadCauseMaximumNodes = 128
)

// Policies share traversal bounds without sharing status authority.
type minerReadCauseBudget struct {
	remaining int
}

// Reject nil receivers before invoking custom unwrap or network methods.
func (self *minerReadCauseBudget) admit(err error, depth int) bool {
	self.remaining--
	if err == nil || self.remaining < 0 || depth > minerReadCauseMaximumDepth {
		return false
	}
	value := reflect.ValueOf(err)
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return !value.IsNil()
	default:
		return true
	}
}
