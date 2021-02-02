package monitor

import (
	"bufio"
	"os/exec"
	"strconv"
	"strings"
	"time"

	kl "github.com/accuknox/KubeArmor/KubeArmor/common"
	kg "github.com/accuknox/KubeArmor/KubeArmor/log"
	tp "github.com/accuknox/KubeArmor/KubeArmor/types"
)

// GetProcessInfoFromHostPid Function
func (mon *ContainerMonitor) GetProcessInfoFromHostPid(log tp.Log, hostPid int32) tp.Log {
	mon.ActiveHostPidMapLock.Lock()

	for id, pidMap := range mon.ActiveHostPidMap {
		for pid, node := range pidMap {
			if hostPid == int32(pid) {
				log.ContainerID = id

				log.PPID = int32(node.PPID)
				log.PID = int32(node.PID)
				log.UID = int32(node.UID)

				break
			}
		}

		if log.ContainerID != "" {
			break
		}
	}

	mon.ActiveHostPidMapLock.Unlock()

	if log.ContainerID == "" {
		time.Sleep(time.Millisecond * 1)

		mon.ActiveHostPidMapLock.Lock()

		for id, pidMap := range mon.ActiveHostPidMap {
			for pid, node := range pidMap {
				if hostPid == int32(pid) {
					log.ContainerID = id

					log.PPID = int32(node.PPID)
					log.PID = int32(node.PID)
					log.UID = int32(node.UID)

					break
				}
			}

			if log.ContainerID != "" {
				break
			}
		}

		mon.ActiveHostPidMapLock.Unlock()
	}

	if log.PPID == 0 {
		log.PPID = -1
		log.PID = -1
		log.UID = -1
	}

	return log
}

// GetContainerInfoFromContainerID Function
func (mon *ContainerMonitor) GetContainerInfoFromContainerID(log tp.Log, profileName string) tp.Log {
	Containers := *(mon.Containers)
	ContainersLock := *(mon.ContainersLock)

	if log.ContainerID != "" {
		ContainersLock.Lock()

		if val, ok := Containers[log.ContainerID]; ok {
			log.NamespaceName = val.NamespaceName
			log.PodName = val.ContainerGroupName
			log.ContainerName = val.ContainerName
		}

		ContainersLock.Unlock()
	} else {
		ContainersLock.Lock()

		for _, container := range Containers {
			if strings.HasPrefix(profileName, container.AppArmorProfile) {
				log.NamespaceName = container.NamespaceName
				log.PodName = container.ContainerGroupName
				log.ContainerID = container.ContainerID
				log.ContainerName = container.ContainerName
				break
			}
		}

		ContainersLock.Unlock()
	}

	if log.ContainerID == "" {
		log.NamespaceName = "NOT_DISCOVERED"
		log.PodName = "NOT_DISCOVERED"
		log.ContainerID = "NOT_DISCOVERED"
		log.ContainerName = "NOT_DISCOVERED"
	}

	return log
}

// UpdateSourceAndResource Function
func (mon *ContainerMonitor) UpdateSourceAndResource(log tp.Log, source, resource string) tp.Log {
	if log.Operation == "Process" {
		log.Source = mon.GetExecPath(log.ContainerID, uint32(log.PPID))
		if log.Source == "" {
			log.Source = source
		}

		log.Resource = mon.GetExecPath(log.ContainerID, uint32(log.PID))
		if log.Resource == "" {
			log.Resource = resource
		} else if !strings.HasPrefix(log.Resource, resource) {
			log.Resource = resource
		}
	} else { // File
		log.Source = mon.GetExecPath(log.ContainerID, uint32(log.PID))
		if log.Source == "" {
			log.Source = source
		}

		log.Resource = resource
	}

	return log
}

// GenerateAuditLog Function
func (mon *ContainerMonitor) GenerateAuditLog(hostPid int32, profileName, source, operation, resource, action, data string) {
	log := tp.Log{}

	log.UpdatedTime = kl.GetDateTimeNow()

	log.HostName = mon.HostName
	log.HostPID = hostPid
	log.Operation = operation

	log = mon.GetProcessInfoFromHostPid(log, hostPid)           // ContainerID, PPID, PID, UID
	log = mon.GetContainerInfoFromContainerID(log, profileName) // NamespaceName, PodName, ContainerName
	log = mon.UpdateSourceAndResource(log, source, resource)    // Source, Resource

	log.Data = data

	if action == "AUDIT" {
		log.Result = "Passed"
	} else {
		log.Result = "Permission denied"
	}

	log = mon.UpdateMatchedPolicy(log, -13)

	if mon.LogFeeder != nil {
		mon.LogFeeder.PushLog(log)
	}
}

// MonitorAuditLogs Function
func (mon *ContainerMonitor) MonitorAuditLogs() {
	logFile := "/KubeArmor/audit/audit.log"

	if kl.IsK8sLocal() {
		logFile = "/var/log/audit/audit.log"
	}

	// monitor audit logs
	cmd := exec.Command("tail", "-f", logFile)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		kg.Err(err.Error())
		return
	}

	if err := cmd.Start(); err != nil {
		kg.Err(err.Error())
		return
	}

	r := bufio.NewReader(stdout)

	for {
		select {
		case <-StopChan:
			stdout.Close()
			return

		default:
			lineBytes, _, err := r.ReadLine()
			if err != nil {
				continue
			}
			line := string(lineBytes)

			if !strings.Contains(line, "type=AVC") {
				continue
			} else if !strings.Contains(line, "DENIED") { // !strings.Contains(line, "AUDIT")
				continue
			} else if !strings.Contains(line, "exec") && !strings.Contains(line, "open") {
				continue
			}

			if mon.IsCOS {
				line = strings.Replace(line, "\\\"", "\"", -1)
			}

			hostPid := int32(0)

			profileName := ""

			source := ""
			operation := ""
			resource := ""
			action := ""

			requested := ""
			denied := ""

			words := strings.Split(line, " ")

			for _, word := range words {
				if strings.HasPrefix(word, "pid=") {
					value := strings.Split(word, "=")
					pid, _ := strconv.Atoi(value[1])
					hostPid = int32(pid)
				} else if strings.HasPrefix(word, "profile=") {
					value := strings.Split(word, "=")
					profileName = strings.Replace(value[1], "\"", "", -1)
				} else if strings.HasPrefix(word, "comm=") {
					value := strings.Split(word, "=")
					source = strings.Replace(value[1], "\"", "", -1)
				} else if strings.HasPrefix(word, "operation=") {
					value := strings.Split(word, "=")
					operation = strings.Replace(value[1], "\"", "", -1)
				} else if strings.HasPrefix(word, "name=") {
					value := strings.Split(word, "=")
					resource = strings.Replace(value[1], "\"", "", -1)
				} else if strings.HasPrefix(word, "apparmor=") {
					value := strings.Split(word, "=")
					action = strings.Replace(value[1], "\"", "", -1)
				} else if strings.HasPrefix(word, "requested_mask=") {
					value := strings.Split(word, "=")
					requested = strings.Replace(value[1], "\"", "", -1)
				} else if strings.HasPrefix(word, "denied_mask=") {
					value := strings.Split(word, "=")
					denied = strings.Replace(value[1], "\"", "", -1)
				}
			}

			if operation == "exec" {
				operation = "Process"
			} else { // open
				operation = "File"
			}

			data := "requested=" + requested
			if denied != "" {
				data = data + " denied=" + denied
			}

			go mon.GenerateAuditLog(hostPid, profileName, source, operation, resource, action, data)
		}
	}
}
