package process

import (
	"context"
	log "github.com/sirupsen/logrus"
	"process-wrapper/pkg/metrics"
	"process-wrapper/pkg/stat"
	"runtime"
	"syscall"
	"time"
)

const (
	GetStatsPeriodSeconds    = 10
	GetStatsFailureThreshold = 3
)

// GetStats monitors process cpu and memory usage
func (c *Command) GetStats(ctx context.Context, pid int) {
	if runtime.GOOS != "linux" {
		log.Warnf("Proc stat export not supported on %v", runtime.GOOS)
		return
	}
	s := stat.NewProcStats(pid, 0, 0)
	var err error
	var cpuSeconds, cpuDelta, memoryUsage float64
	var failedStatCollection int
	log.WithFields(log.Fields{"name": c.Name, "pid": pid}).Warnf("Memory limit: %v", c.MemoryLimitMB)
	for {
		select {
		case <-ctx.Done():
			log.WithFields(log.Fields{"name": c.Name, "pid": pid}).Debugf("Stop stats worker")
			return
		default:
			cpuSeconds, cpuDelta, memoryUsage, err = s.Stat()
			if err != nil || cpuSeconds+cpuDelta+memoryUsage == 0 {
				failedStatCollection++
				if failedStatCollection >= GetStatsFailureThreshold {
					log.WithFields(log.Fields{"name": c.Name, "pid": pid}).Errorf("Get stats retries is excided")
					return
				}
				log.WithFields(log.Fields{"name": c.Name, "pid": pid}).Warnf("Failed to get process stats retry=%v, %v", failedStatCollection, err)
				time.Sleep(time.Second)
				continue
			}
			if cpuSeconds+cpuDelta+memoryUsage == 0 {
				log.WithFields(log.Fields{"name": c.Name, "pid": pid}).Warnf("Unknown process stats")
				time.Sleep(time.Second * GetStatsPeriodSeconds)
				continue
			}
			failedStatCollection = 0
			metrics.SidecarCmdCPUSeconds.WithLabelValues(c.Name).Add(cpuDelta)
			metrics.SidecarCmdCPUSecondsGauge.WithLabelValues(c.Name).Set(cpuSeconds)
			metrics.SidecarCmdMemoryUsageGauge.WithLabelValues(c.Name).Set(memoryUsage)

			log.WithFields(log.Fields{"name": c.Name, "pid": pid}).Debugf("CPU: %v, Delta: %v, Memory: %v", cpuSeconds, cpuDelta, memoryUsage)

			// Handle memory limit
			if c.MemoryLimitMB > 0 && memoryUsage/1000000 > c.MemoryLimitMB {
				log.WithFields(log.Fields{"name": c.Name, "pid": pid}).Warnf("Out of memory %v > %v, killing the process", memoryUsage/1000000, c.MemoryLimitMB)
				c.s.SetKillState(c.Name, true)
				err = syscall.Kill(pid, syscall.SIGKILL)
				if err != nil {
					log.WithFields(log.Fields{"name": c.Name, "pid": pid}).Warnf("Terminate process failure %v", err)
				}
				metrics.SidecarCmdReadinessCount.WithLabelValues("oom", c.Name, c.String()).Inc()
			}
			time.Sleep(time.Second * GetStatsPeriodSeconds)
		}
	}
}
