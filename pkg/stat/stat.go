package stat

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	CLKTCK   = 100
	PAGESIZE = 4096
)

type ProcStat struct {
	pid        string
	clktck     float64
	pageSize   float64
	cpuSeconds float64
}

func NewProcStats(pid int, clktck, pageSize float64) *ProcStat {
	return &ProcStat{
		pid: fmt.Sprintf("%v", pid),
		clktck: func(clktck float64) float64 {
			if clktck == 0 {
				return CLKTCK
			}
			return clktck
		}(clktck),
		pageSize: func(pageSize float64) float64 {
			if pageSize == 0 {
				return PAGESIZE
			}
			return pageSize
		}(pageSize),
	}
}

// Stat returns pid cpu_seconds, cpu_seconds_delta, memory bytes, and 0,0,0 in case of error
func (p *ProcStat) Stat() (float64, float64, float64, error) {
	statFile, err := os.ReadFile("/proc/" + p.pid + "/stat")
	if err != nil {
		return 0, 0, 0, fmt.Errorf("cannot read stat file %v", err)
	}

	statFileString := string(statFile)
	i := strings.Index(statFileString, ")")
	if i == -1 {
		return 0, 0, 0, fmt.Errorf("cannot find ')' in stat file")
	}
	columns := strings.Split(statFileString[i+1:], " ")
	if len(columns) < 23 {
		return 0, 0, 0, fmt.Errorf("cannot parse stat file, %v", err)
	}
	cpuSec := (sTof64(columns[12]) + sTof64(columns[13])) / p.clktck
	memBytes := sTof64(columns[22]) * p.pageSize

	delta := cpuSec - p.cpuSeconds
	p.cpuSeconds = cpuSec
	return cpuSec, delta, memBytes, nil
}

func sTof64(input string) float64 {
	value, err := strconv.ParseFloat(input, 64)
	if err != nil {
		fmt.Printf("Cannot convert string %v to float, %v", input, err)
		return 0
	}
	return value
}
