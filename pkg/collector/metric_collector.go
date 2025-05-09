/*
Copyright 2021.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package collector

import (
	"os"
	"syscall"
	"time"

	"github.com/sustainable-computing-io/kepler/pkg/bpf"
	"github.com/sustainable-computing-io/kepler/pkg/cgroup"
	"github.com/sustainable-computing-io/kepler/pkg/collector/energy"
	"github.com/sustainable-computing-io/kepler/pkg/collector/resourceutilization/accelerator"
	resourceBpf "github.com/sustainable-computing-io/kepler/pkg/collector/resourceutilization/bpf"
	"github.com/sustainable-computing-io/kepler/pkg/collector/stats"
	"github.com/sustainable-computing-io/kepler/pkg/config"
	"github.com/sustainable-computing-io/kepler/pkg/model"
	acc "github.com/sustainable-computing-io/kepler/pkg/sensors/accelerator"
	"github.com/sustainable-computing-io/kepler/pkg/utils"

	"k8s.io/klog/v2"
)

const (
	maxInactiveContainers = 10
	maxInactiveVM         = 3
)

type Collector struct {
	// NodeStats holds all node energy and resource usage metrics
	NodeStats stats.NodeStats

	// ProcessStats hold all process energy and resource usage metrics
	ProcessStats map[uint64]*stats.ProcessStats

	// ContainerStats holds the aggregated processes metrics for all containers
	ContainerStats map[string]*stats.ContainerStats

	// VMStats holds the aggregated processes metrics for all virtual machines
	VMStats map[string]*stats.VMStats

	// bpfExporter handles gathering metrics from bpf probes
	bpfExporter bpf.Exporter
	// bpfSupportedMetrics holds the supported metrics by the bpf exporter
	bpfSupportedMetrics bpf.SupportedMetrics
}

func NewCollector(bpfExporter bpf.Exporter) *Collector {
	bpfSupportedMetrics := bpfExporter.SupportedMetrics()
	c := &Collector{
		NodeStats:           *stats.NewNodeStats(),
		ContainerStats:      map[string]*stats.ContainerStats{},
		ProcessStats:        map[uint64]*stats.ProcessStats{},
		VMStats:             map[string]*stats.VMStats{},
		bpfExporter:         bpfExporter,
		bpfSupportedMetrics: bpfSupportedMetrics,
	}
	return c
}

func (c *Collector) Initialize() error {
	// For local estimator, there is endpoint provided, thus we should let
	// model component decide whether/how to init
	model.CreatePowerEstimatorModels(
		stats.GetProcessFeatureNames(),
	)

	return nil
}

// Update updates the node and container energy and resource usage metrics
func (c *Collector) Update() {
	start := time.Now()
	// reset the previous collected value because not all process will have new data
	// that is, a process that was inactive will not have any update but we need to set its metrics to 0
	c.resetDeltaValue()

	// collect process resource utilization and aggregate it per node, container and VMs
	c.updateResourceUtilizationMetrics()

	// collect node power and estimate process power
	c.UpdateEnergyUtilizationMetrics()

	c.printDebugMetrics()
	klog.V(5).Infof("Collector Update elapsed time: %s", time.Since(start))
}

// resetDeltaValue resets existing podEnergy previous curr value
func (c *Collector) resetDeltaValue() {
	c.NodeStats.ResetDeltaValues()
	for _, v := range c.ProcessStats {
		v.ResetDeltaValues()
	}
	if config.IsExposeContainerStatsEnabled() {
		for _, v := range c.ContainerStats {
			v.ResetDeltaValues()
		}
	}
	if config.IsExposeVMStatsEnabled() {
		for _, v := range c.VMStats {
			v.ResetDeltaValues()
		}
	}
}

func (c *Collector) UpdateEnergyUtilizationMetrics() {
	c.UpdateNodeEnergyUtilizationMetrics()
	c.UpdateProcessEnergyUtilizationMetrics()
	// aggregate the process metrics per container and/or VMs
	c.AggregateProcessEnergyUtilizationMetrics()
}

// UpdateNodeEnergyUtilizationMetrics collects real-time node resource power utilization
// if there is no real-time power meter, use the container resource usage metrics to estimate the node's resource power
func (c *Collector) UpdateNodeEnergyUtilizationMetrics() {
	energy.UpdateNodeEnergyMetrics(&c.NodeStats)
}

// UpdateProcessEnergyUtilizationMetrics estimates the process energy consumption using its resource utilization and the node components energy consumption
func (c *Collector) UpdateProcessEnergyUtilizationMetrics() {
	energy.UpdateProcessEnergy(c.ProcessStats, &c.NodeStats)
}

func (c *Collector) updateResourceUtilizationMetrics() {
	// NOTE: no node resource utilization metrics to aggregate
	c.updateProcessResourceUtilizationMetrics()

	// aggregate processes' resource utilization metrics to containers, virtual machines and nodes
	c.AggregateProcessResourceUtilizationMetrics()
}

func (c *Collector) updateProcessResourceUtilizationMetrics() {
	// update process metrics regarding the resource utilization to be used to calculate the energy consumption
	// we first updates the bpf which is responsible to include new processes in the ProcessStats collection
	resourceBpf.UpdateProcessBPFMetrics(c.bpfExporter, c.ProcessStats)
	if config.IsGPUEnabled() {
		if acc.GetActiveAcceleratorByType(config.GPU) != nil {
			accelerator.UpdateProcessGPUUtilizationMetrics(c.ProcessStats)
		}
	}
}

// AggregateProcessResourceUtilizationMetrics aggregates processes' resource utilization metrics to containers, virtual machines and nodes
func (c *Collector) AggregateProcessResourceUtilizationMetrics() {
	foundContainer := make(map[string]bool)
	foundVM := make(map[string]bool)
	for _, process := range c.ProcessStats {
		if process.IdleCounter > 0 {
			// if the process metrics were not updated for multiple iterations, very if the process still exist, otherwise delete it from the map
			c.handleIdlingProcess(process)
		}
		for metricName, resource := range process.ResourceUsage {
			for id := range resource {
				delta := resource[id].GetDelta() // currently the process metrics are single socket

				// aggregate metrics per container
				if config.IsExposeContainerStatsEnabled() {
					if process.ContainerID != "" {
						c.createContainerStatsIfNotExist(process.ContainerID, process.CGroupID, process.PID, config.EnabledEBPFCgroupID())
						c.ContainerStats[process.ContainerID].ResourceUsage[metricName].AddDeltaStat(id, delta)
						foundContainer[process.ContainerID] = true
					}
				}

				// aggregate metrics per Virtual Machine
				if config.IsExposeVMStatsEnabled() {
					if process.VMID != "" {
						if _, ok := c.VMStats[process.VMID]; !ok {
							c.VMStats[process.VMID] = stats.NewVMStats(process.PID, process.VMID)
						}
						c.VMStats[process.VMID].ResourceUsage[metricName].AddDeltaStat(id, delta)
						foundVM[process.VMID] = true
					}
				}

				// aggregate metrics from all process to represent the node resource utilization
				c.NodeStats.ResourceUsage[metricName].AddDeltaStat(id, delta)
			}
		}
	}

	// clean up the cache
	// TODO: improve the removal of deleted containers from ContainerStats. Currently we verify the maxInactiveContainers using the found map
	if config.IsExposeContainerStatsEnabled() {
		c.handleInactiveContainers(foundContainer)
	}
	if config.IsExposeVMStatsEnabled() {
		c.handleInactiveVM(foundVM)
	}
}

// handleInactiveProcesses
func (c *Collector) handleIdlingProcess(pStat *stats.ProcessStats) {
	proc, _ := os.FindProcess(int(pStat.PID))
	err := proc.Signal(syscall.Signal(0))
	if err != nil {
		// delete if the process does not exist anymore
		delete(c.ProcessStats, pStat.PID)
		return
	}
}

// handleInactiveContainers
func (c *Collector) handleInactiveContainers(foundContainer map[string]bool) {
	numOfInactive := len(c.ContainerStats) - len(foundContainer)
	if numOfInactive > maxInactiveContainers {
		aliveContainers, err := cgroup.GetAliveContainers()
		if err != nil {
			klog.V(5).Infoln(err)
			return
		}
		for containerID := range c.ContainerStats {
			if containerID == utils.SystemProcessName || containerID == utils.KernelProcessName {
				continue
			}
			if _, found := aliveContainers[containerID]; !found {
				delete(c.ContainerStats, containerID)
			}
		}
	}
}

// handleInactiveVirtualMachine
func (c *Collector) handleInactiveVM(foundVM map[string]bool) {
	numOfInactive := len(c.VMStats) - len(foundVM)
	if numOfInactive > maxInactiveVM {
		for vmID := range c.VMStats {
			if _, found := foundVM[vmID]; !found {
				delete(c.VMStats, vmID)
			}
		}
	}
}

// AggregateProcessEnergyUtilizationMetrics aggregates processes' utilization metrics to containers and virtual machines
func (c *Collector) AggregateProcessEnergyUtilizationMetrics() {
	for _, process := range c.ProcessStats {
		for metricName, stat := range process.EnergyUsage {
			for id := range stat {
				delta := stat[id].GetDelta() // currently the process metrics are single socket

				// aggregate metrics per container
				if config.IsExposeContainerStatsEnabled() {
					if process.ContainerID != "" {
						c.createContainerStatsIfNotExist(process.ContainerID, process.CGroupID, process.PID, config.EnabledEBPFCgroupID())
						c.ContainerStats[process.ContainerID].EnergyUsage[metricName].AddDeltaStat(id, delta)
					}
				}

				// aggregate metrics per Virtual Machine
				if config.IsExposeVMStatsEnabled() {
					if process.VMID != "" {
						if _, ok := c.VMStats[process.VMID]; !ok {
							c.VMStats[process.VMID] = stats.NewVMStats(process.PID, process.VMID)
						}
						c.VMStats[process.VMID].EnergyUsage[metricName].AddDeltaStat(id, delta)
					}
				}
			}
		}
	}
}

func (c *Collector) printDebugMetrics() {
	// check the log verbosity level before iterating in all container
	if klog.V(3).Enabled() {
		if config.IsExposeContainerStatsEnabled() {
			for _, v := range c.ContainerStats {
				klog.V(3).Infoln(v)
			}
		}
		klog.V(3).Infoln(c.NodeStats.String())
	}
}
