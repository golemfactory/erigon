package estimate

import (
	"runtime"

	"github.com/c2h5oh/datasize"
	"github.com/ledgerwatch/erigon-lib/common/cmp"
	"github.com/pbnjay/memory"
)

type estimatedRamPerWorker datasize.ByteSize

// Workers - return max workers amount based on total Memory/CPU's and estimated RAM per worker
func (r estimatedRamPerWorker) Workers() int {
	maxWorkersForGivenMemory := memory.TotalMemory() / uint64(r)
	maxWorkersForGivenCPU := runtime.NumCPU() - 1 // reserve 1 cpu for "work-producer thread", also IO software on machine in cloud-providers using 1 CPU
	return cmp.InRange(1, maxWorkersForGivenCPU, int(maxWorkersForGivenMemory))
}

const (
	IndexSnapshot     = estimatedRamPerWorker(2 * datasize.MB) //elias-fano index building is single-threaded
	CompressSnapshot  = estimatedRamPerWorker(1 * datasize.GB) //1-file-compression is multi-threaded
	ReconstituteState = estimatedRamPerWorker(4 * datasize.GB) //state-reconstitution is multi-threaded
)
