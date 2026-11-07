//go:build unix

package main

func diagAvailableBytes[T ~int64 | ~uint64](blocks T, blockSize uint64) uint64 {
	if blocks < 0 {
		return 0
	}
	return uint64(blocks) * blockSize
}
