package vmware

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"code.vorteil.io/vorteil/tools/cli/pkg/daemon/api"
	"code.vorteil.io/vorteil/tools/cli/pkg/daemon/graph"
	"github.com/vorteil/vorteil/pkg/vio"
	"github.com/vorteil/vorteil/pkg/vcfg"
	"github.com/vorteil/vorteil/pkg/virtualizers"
	logger "github.com/vorteil/vorteil/pkg/virtualizers/logging"
	"github.com/vorteil/vorteil/pkg/vcfg"
	"github.com/thanhpk/randstr"
)

// vmwareType workstation by default switch to fusion when on a darwin system
var vmwareType = "workstation"

func init() {
	if runtime.GOOS == "darwin" {
		vmwareType = "fusion"
	}
}

// Virtualizer is a struct which will implement the interface so the manager can create VMs
type Virtualizer struct {
	id           string         // unique hash for pipe and folder names.
	name         string         // name of the vm
	pname        string         // name of virtualizer spawned from
	state        string         // the state of the vm
	headless     bool           // bool to show or not to show the gui
	created      time.Time      // time the vm was created
	folder       string         // path to the folder containing vmx, disk for vm
	disk         *os.File       // the disk the vm is running
	vmxPath      string         // the vmx file workstation will use
	networkType  string         // the type of network the vm spawns on
	virtLogger   *logger.Logger // virtualizer logger outputs what is executed
	source       interface{}    //details about how the source was created using api.source struct
	serialLogger *logger.Logger // serial output logger for app that gets run
	startCommand *exec.Cmd      // The execute command to start the vmware instance
	sock         net.Conn       // net connection to read serial from

	routes    []api.NetworkInterface
	config    *vcfg.VCFG
	subServer *graph.Graph

	vmdrive string // store disks in this directory

}

// lookForIp looks for IP via the screen output as vmware spawns on different IPs
func (v *Virtualizer) lookForIP() {
	sub := v.serialLogger.Subscribe()
	inbox := sub.Inbox()
	var msg string
	timer := false
	msgWrote := false
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case logdata, _ := <-inbox:
			msg += string(logdata)
			if strings.TrimSpace(msg) != "" && strings.Contains(msg, "ip") {
				msgWrote = true
			}
		case <-ticker.C:
			if msgWrote {
				// sleep slightly so we get all the IPS
				time.Sleep(time.Second * 1)
				timer = true
			}
		// after 30 seconds break out of for loop for memory resolving
		case <-time.After(time.Second * 30):
			timer = true
		}
		if timer {
			break
		}
	}
	var ips []string
	lines := strings.Split(msg, "\r\n")
	for _, line := range lines {
		if virtualizers.IPRegex.MatchString(line) {
			if strings.Contains(line, "ip") {
				split := strings.Split(line, ":")
				if len(split) > 1 {
					ips = append(ips, strings.TrimSpace(split[1]))
				}
			}

		}
	}

	if len(ips) > 0 {
		for i, route := range v.routes {
			for j, port := range route.HTTP {
				v.routes[i].HTTP[j].Address = fmt.Sprintf("%s:%s", ips[i], port.Port)
			}
			for j, port := range route.HTTPS {
				v.routes[i].HTTPS[j].Address = fmt.Sprintf("%s:%s", ips[i], port.Port)
			}
			for j, port := range route.TCP {
				v.routes[i].TCP[j].Address = fmt.Sprintf("%s:%s", ips[i], port.Port)
			}
			for j, port := range route.UDP {
				v.routes[i].UDP[j].Address = fmt.Sprintf("%s:%s", ips[i], port.Port)
			}
		}
	}
}

// RemoveEntry from vmware inventory
func (v *Virtualizer) RemoveEntry() error {
	env, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		env = os.Getenv("APPDATA")
	}
	pathVMware := filepath.ToSlash(filepath.Join(env, "VMware/inventory.vmls"))
	if runtime.GOOS != "windows" {
		pathVMware = filepath.ToSlash(filepath.Join(env, ".vmware/inventory.vmls"))
	}

	file, err := ioutil.ReadFile(pathVMware)
	if err != nil {
		return err
	}

	keys := make([]string, 0)
	found := false
	// Fetch what lines i need to remove from the file
	lines := strings.Split(string(file), "\n")
	for _, line := range lines {
		if strings.Contains(line, v.vmxPath) {
			id := strings.TrimSpace(strings.Split(line, "=")[0])
			removeType := strings.Split(id, ".")[0]
			keys = append(keys, removeType)
			found = true
		}
	}

	// if not found under .vorteild directory try with normal spot does not need to happen on windows as vmware is always open
	if !found && runtime.GOOS != "windows" {
		pathVMware = filepath.ToSlash(filepath.Join(filepath.Dir(env), ".vmware/inventory.vmls"))
		file, err = ioutil.ReadFile(pathVMware)
		if err != nil {
			return err
		}
		// Fetch what lines i need to remove from the file
		lines := strings.Split(string(file), "\n")
		for _, line := range lines {
			if strings.Contains(line, v.vmxPath) {
				id := strings.TrimSpace(strings.Split(line, "=")[0])
				removeType := strings.Split(id, ".")[0]
				keys = append(keys, removeType)
			}
		}
	}

	// open the file for editing.
	f, err := os.Create(pathVMware)
	if err != nil {
		return err
	}

	for _, line := range lines {
		lineFound := false
		for _, key := range keys {
			if strings.HasPrefix(line, key) {
				lineFound = true
			}
		}

		// check index.count line to adjust it with removing one
		if strings.HasPrefix(line, "index.count") {
			count := strings.TrimSpace(strings.Split(line, "=")[1])
			count = strings.Trim(count, "\"")
			// remove one from index as were deleting one vm from vmware
			ncount, err := strconv.Atoi(count)
			if err != nil {
				return err
			}
			ncount--
			f.WriteString(fmt.Sprintf("index.count = \"%s\"", strconv.Itoa(ncount)))
			lineFound = true
		}
		// If line didn't hit any checks write the file back in as usual
		if !lineFound {
			f.WriteString(line)
		}
	}
	defer f.Close()

	return nil
}

// Close deletes and cleans up the VM
func (v *Virtualizer) Close(force bool) error {
	v.log("debug", "Deleting VM")
	if force && v.state != virtualizers.Ready {
		err := v.ForceStop()
		if err != nil {
			return err
		}
	} else if v.state != virtualizers.Ready {
		err := v.Stop()
		if err != nil {
			return err
		}
	}

	command := exec.Command("vmrun", "-T", vmwareType, "deleteVM", v.vmxPath)
	output, err := v.execute(command)
	if err != nil {
		if !strings.Contains(err.Error(), "4294967295") {
			if runtime.GOOS == "darwin" && !v.headless {
				if strings.Contains(err.Error(), "is in use") {
					v.log("error", "%s (if running with gui make sure its closed)", err.Error())
					return fmt.Errorf("%s (if running with gui make sure its closed)", err.Error())
				}
			}
			return err
		}
	}
	if len(output) > 0 {
		v.log("info", "%s", output)
	}

	v.state = virtualizers.Deleted
	v.subServer.SubServer.Publish(graph.VMUpdater)

	if v.sock != nil {
		v.sock.Close()
	}
	v.disk.Close()

	if !v.headless {
		err = v.RemoveEntry()
		if err != nil {
			// the gui on mac requires you to remove it before you can delete so returning this error makes no sense
			if runtime.GOOS != "darwin" {
				return err
			}
		}
	}

	virtualizers.ActiveVMs.Delete(v.name)
	err = os.RemoveAll(v.folder)
	if err != nil {
		return err
	}
	return nil
}

// Detach removes vm from active vm list
func (v *Virtualizer) Detach(source string) error {
	if v.state != virtualizers.Ready {
		return errors.New("virtual machine must be in ready state to detach")
	}

	err := os.MkdirAll(filepath.Join(source, v.name), 0777)
	if err != nil {
		return err
	}

	cmd := exec.Command("vmrun", "-T", vmwareType, "clone", v.vmxPath, filepath.Join(source, v.name, filepath.Base(v.vmxPath)), "full")
	_, err = v.execute(cmd)
	if err != nil {
		if strings.Contains(err.Error(), "4294967295") {
			return errors.New("vm contents already exist at location")
		}
		return err
	}
	
	command := exec.Command("vmrun", "-T", vmwareType, "deleteVM", v.vmxPath)
	output, err := v.execute(command)
	if err != nil {
		if !strings.Contains(err.Error(), "4294967295") {
			if runtime.GOOS == "darwin" && !v.headless {
				if strings.Contains(err.Error(), "is in use") {
					v.log("error", "%s (if running with gui make sure its closed)", err.Error())
					return fmt.Errorf("%s (if running with gui make sure its closed)", err.Error())
				}
			}
			return err
		}
	}
	if len(output) > 0 {
		v.log("info", "%s", output)
	}

	v.state = virtualizers.Deleted
	v.subServer.SubServer.Publish(graph.VMUpdater)

	if v.sock != nil {
		v.sock.Close()
	}
	v.disk.Close()

	virtualizers.ActiveVMs.Delete(v.name)
	err = os.RemoveAll(v.folder)
	if err != nil {
		return err
	}
	return nil
}

// ForceStop stop the vm without shutting down mainly used when the daemon gets powered off
func (v *Virtualizer) ForceStop() error {
	command := exec.Command("vmrun", "-T", vmwareType, "stop", v.vmxPath, "hard")
	output, err := v.execute(command)
	if err != nil {
		if !strings.Contains(err.Error(), "4294967295") {
			return err
		}
	}
	if len(output) > 0 {
		v.log("info", "%s", output)
	}
	v.state = virtualizers.Ready
	v.subServer.SubServer.Publish(graph.VMUpdater)

	return nil
}

// Stop the vm with sigint through the hypervisor
func (v *Virtualizer) Stop() error {
	v.log("debug", "Stopping VM")
	if v.state != virtualizers.Ready {
		v.state = virtualizers.Changing
		command := exec.Command("vmrun", "-T", vmwareType, "stop", v.vmxPath)
		output, err := v.execute(command)
		if err != nil {
			if !strings.Contains(err.Error(), "4294967295") {
				return err
			}
		}
		if len(output) > 0 {
			v.log("info", "%s", output)
		}

		v.state = virtualizers.Ready
		v.subServer.SubServer.Publish(graph.VMUpdater)

	}
	return nil
}

// execute is a generic wrapper function for executing commands
func (v *Virtualizer) execute(cmd *exec.Cmd) (string, error) {
	v.log("info", "Executing %s", cmd.Args)
	resp, err := cmd.CombinedOutput()
	if err != nil {

		if err.Error() == "" || err.Error() == "exit status 255" {
			return "", errors.New(string(resp))
		}
		return "", err
	}
	output := string(resp)
	return output, nil
}

// Start the vm
func (v *Virtualizer) Start() error {
	v.log("debug", "Starting VM")
	v.startCommand = exec.Command(v.startCommand.Args[0], v.startCommand.Args[1:]...)
	switch v.State() {
	case "ready":
		go v.initLogs()

		output, err := v.execute(v.startCommand)
		if err != nil {
			v.log("error", "Error starting vm: %v", err)
			return err
		}
		if len(output) > 0 {
			v.log("info", "%s", output)
		}
		v.state = virtualizers.Alive
		v.subServer.SubServer.Publish(graph.VMUpdater)

		go v.lookForIP()
		go v.checkRunning()

	default:
		return fmt.Errorf("cannot start vm in state '%s'", v.State())
	}
	return nil
}

// Logs returns the virtualizer logger
func (v *Virtualizer) Logs() *logger.Logger {
	return v.virtLogger
}

// Serial returns the serial logger
func (v *Virtualizer) Serial() *logger.Logger {
	return v.serialLogger
}

// State returns the state of the virtual machine
func (v *Virtualizer) State() string {
	return v.state
}

// Type returns the type of the virtualizer
func (v *Virtualizer) Type() string {
	return VirtualizerID
}

// Initialize creates the virtualizer and appends needed data from the Config
func (v *Virtualizer) Initialize(data []byte) error {
	c := new(Config)
	err := c.Unmarshal(data)
	if err != nil {
		return err
	}
	v.networkType = c.NetworkType
	v.headless = c.Headless
	return nil
}

// operation is the job progress that gets tracked via APIs
type operation struct {
	finishedLock sync.Mutex
	isFinished   bool
	*Virtualizer
	Logs   chan string
	Status chan string
	Error  chan error
	ctx    context.Context
}

// log writes a log to the channel for the job
func (o *operation) log(text string, v ...interface{}) {
	o.Logs <- fmt.Sprintf(text, v...)
}

// finished completes the operation and lets the user know and cleans up channels
func (o *operation) finished(err error) {
	o.finishedLock.Lock()
	defer o.finishedLock.Unlock()
	if o.isFinished {
		return
	}
	o.isFinished = true

	if err != nil {
		o.Logs <- fmt.Sprintf("Error: %v", err)
		o.Status <- fmt.Sprintf("Failed: %v", err)
		o.Error <- err
	}

	close(o.Logs)
	close(o.Status)
	close(o.Error)
}

// updateStatus updates the status of the job to provide more feedback to the user currently reading the job.
func (o *operation) updateStatus(text string) {
	o.Status <- text
	o.Logs <- text
}

// log writes a log line to the logger and adds prefix and suffix depending on what type of log was sent.
func (v *Virtualizer) log(logType string, text string, args ...interface{}) {
	switch logType {
	case "error":
		text = fmt.Sprintf("%s%s%s\n", "\033[31m", text, "\033[0m")
	case "warning":
		text = fmt.Sprintf("%s%s%s\n", "\033[33m", text, "\033[0m")
	case "info":
		text = fmt.Sprintf("%s%s%s\n", "\u001b[37;1m", text, "\u001b[0m")
	default:
		text = fmt.Sprintf("%s\n", text)
	}
	v.virtLogger.Write([]byte(fmt.Sprintf(text, args...)))
}

// Prepare sets the fields and arguments to spawn the virtual machine
func (v *Virtualizer) Prepare(args *virtualizers.PrepareArgs) *virtualizers.VirtualizeOperation {
	op := new(operation)
	v.name = args.Name
	v.pname = args.PName
	v.subServer = args.Subserver
	v.subServer.SubServer.Publish(graph.VMUpdater)
	v.vmdrive = args.VMDrive
	v.created = time.Now()
	v.config = args.Config
	v.source = args.Source
	v.virtLogger = logger.NewLogger(2048)
	v.serialLogger = logger.NewLogger(2048 * 10)
	v.routes = v.Routes()
	v.log("debug", "Preparing VM")

	op.Logs = make(chan string, 128)
	op.Error = make(chan error, 1)
	op.Status = make(chan string, 10)
	op.ctx = args.Context

	op.Virtualizer = v

	o := new(virtualizers.VirtualizeOperation)
	o.Logs = op.Logs
	o.Error = op.Error
	o.Status = op.Status

	go op.prepare(args)

	return o
}

// Download returns the disk as a file.File
func (v *Virtualizer) Download() (file.File, error) {
	v.log("debug", "Downloading Disk")

	if !(v.state == virtualizers.Ready) {
		return nil, fmt.Errorf("the machine must be in a stopped or ready state")
	}

	f, err := file.LazyOpen(v.disk.Name())
	if err != nil {
		return nil, err
	}

	return f, nil
}

// ConvertToVM is a wrapper function that provides us to use the old APIs
func (v *Virtualizer) ConvertToVM() interface{} {

	info := v.config.Info
	vm := v.config.VM
	system := v.config.System
	programs := make([]api.ProgramSummaries, 0)

	for _, p := range v.config.Programs {
		programs = append(programs, api.ProgramSummaries{
			Binary: p.Binary,
			Args:   string(p.Args),
			Env:    p.Env,
		})
	}

	machine := &api.VirtualMachine{
		ID:       v.name,
		Author:   info.Author,
		CPUs:     int(vm.CPUs),
		RAM:      vm.RAM,
		Disk:     vm.DiskSize,
		Created:  v.created,
		Date:     info.Date.Time(),
		Networks: v.routes,
		Kernel:   vm.Kernel.String(),
		Name:     info.Name,
		Summary:  info.Summary,
		Source:   v.source.(api.Source),
		URL:      string(info.URL),
		Version:  info.Version,
		Programs: programs,
		Hostname: system.Hostname,
		Platform: v.pname,
		Status:   v.state,
	}

	return machine
}

// prepare sets the fields and arguments to spawn the virtual machine
func (o *operation) prepare(args *virtualizers.PrepareArgs) {
	var returnErr error

	o.updateStatus(fmt.Sprintf("Preparing VMware...."))
	defer func() {
		o.finished(returnErr)
	}()

	executable, err := virtualizers.GetExecutable(VirtualizerID)
	if err != nil {
		returnErr = err
		return
	}
	o.state = "initializing"
	o.id = randstr.Hex(5)
	o.folder = filepath.Join(o.vmdrive, fmt.Sprintf("%s-%s", o.id, o.Type()))

	// create vm folder
	err = os.MkdirAll(o.folder, os.ModePerm)
	if err != nil {
		returnErr = err
		return
	}

	// copy disk to folder
	f, err := os.Create(filepath.Join(o.folder, o.name+".vmdk"))
	if err != nil {
		returnErr = err
		return
	}

	_, err = io.Copy(f, args.Image)
	if err != nil {
		returnErr = err
		return
	}

	defer f.Close()
	o.disk = f

	// generate vmx'

	// align size to 4 MiB
	o.config.VM.RAM.Align(size.MiB * 4)

	vmxString := GenerateVMX(strconv.Itoa(int(o.config.VM.CPUs)), strconv.Itoa(o.config.VM.RAM.Units(size.MiB)), o.disk.Name(), o.name, o.folder, len(o.routes), o.networkType, o.id)
	// o.Virtualizer.log("info", "VMX Start:\n%s\nVMX End", vmxString)

	vmxPath := filepath.Join(o.folder, o.name+".vmx")
	o.vmxPath = vmxPath
	err = ioutil.WriteFile(vmxPath, []byte(vmxString), os.ModePerm)
	if err != nil {
		returnErr = err
		return
	}

	argsC := []string{"-T", vmwareType, "start", o.vmxPath}
	if o.headless {
		argsC = append(argsC, "nogui")
	}

	o.startCommand = exec.Command(executable, argsC...)

	_, loaded := virtualizers.ActiveVMs.LoadOrStore(o.name, o.Virtualizer)
	if loaded {
		returnErr = errors.New("virtual machine already exists")
		return
	}

	o.state = "ready"

	if args.Start {
		err = o.Start()
		if err != nil {
			o.Virtualizer.log("error", "Error starting vm: %v", err)
		}
	}
}

// Poll to check if its still running
func (v *Virtualizer) checkRunning() {
	for {
		running, err := v.isRunning()
		if err != nil {
			v.log("error", "Checking Running State: %s", err)
			return
		}
		if !running {
			v.state = virtualizers.Ready
			v.subServer.SubServer.Publish(graph.VMUpdater)

			break
		}
		time.Sleep(time.Second * 1)
	}
}

// Checks if the vm is still running as vmrun does not come with state management.
func (v *Virtualizer) isRunning() (bool, error) {
	running := false

	command := exec.Command("vmrun", "list")
	var errS bytes.Buffer
	command.Stdout = &errS
	err := command.Run()
	if err != nil {
		return running, err
	}

	output := fmt.Sprint(command.Stdout)
	lines := strings.Split(output, "\n")
	vms, _ := strconv.Atoi(strings.Split(string(lines[0]), ": ")[1])
	// try with carriage return for windows
	if vms == 0 {
		lines = strings.Split(string(output), "\r\n")
		vms, _ = strconv.Atoi(strings.Split(string(lines[0]), ": ")[1])
	}
	vmxFile, err := os.Stat(v.vmxPath)
	if err != nil {
		return running, err
	}

	for i := 0; i < vms; i++ {
		vmxFile2, err := os.Stat(lines[i+1])
		if err != nil {
			continue
		}
		if os.SameFile(vmxFile, vmxFile2) {
			running = true
			break
		}
		continue
	}
	return running, nil
}

// Routes converts the VCFG.routes to the apiNetworkInterface which allows
// us to easiler return to currently written graphql APIs
func (v *Virtualizer) Routes() []api.NetworkInterface {

	routes := virtualizers.Routes{}
	var nics = v.config.Networks
	for i, nic := range nics {
		if nic.IP == "" {
			continue
		}
		protocols := []string{
			"udp",
			"tcp",
			"http",
			"https",
		}
		portLists := [][]string{
			nic.UDP,
			nic.TCP,
			nic.HTTP,
			nic.HTTPS,
		}
		for j := 0; j < len(protocols); j++ {
			protocol := protocols[j]
			ports := portLists[j]
			if routes.NIC[i].Protocol == nil {
				routes.NIC[i].Protocol = make(map[virtualizers.NetworkProtocol]*virtualizers.NetworkProtocolPorts)
			}
			if protocol == "" {
				protocol = "http"
			}
			p := virtualizers.NetworkProtocol(protocol)
			existingPorts, ok := routes.NIC[i].Protocol[p]
			if !ok {
				existingPorts = &virtualizers.NetworkProtocolPorts{
					Port: make(map[string]*virtualizers.NetworkRoute),
				}
			}
			for _, port := range ports {
				existingPorts.Port[port] = new(virtualizers.NetworkRoute)
			}
			routes.NIC[i].Protocol[p] = existingPorts
		}
	}
	apiNics := make([]api.NetworkInterface, 0)
	for i, net := range v.config.Networks {
		newNetwork := api.NetworkInterface{
			Name:    "",
			IP:      net.IP,
			Mask:    net.Mask,
			Gateway: net.Gateway,
		}
		for _, port := range net.UDP {
			var addr string
			if len(routes.NIC) > i {
				nic := routes.NIC[i]
				if proto, ok := nic.Protocol["udp"]; ok {
					if pmap, ok := proto.Port[port]; ok {
						addr = pmap.Address
					}
				}
			}
			newNetwork.UDP = append(newNetwork.UDP, api.RouteMap{
				Port:    port,
				Address: addr,
			})
		}
		for _, port := range net.TCP {
			var addr string
			if len(routes.NIC) > i {
				nic := routes.NIC[i]
				if proto, ok := nic.Protocol["tcp"]; ok {
					if pmap, ok := proto.Port[port]; ok {
						addr = pmap.Address
					}
				}
			}
			newNetwork.TCP = append(newNetwork.TCP, api.RouteMap{
				Port:    port,
				Address: addr,
			})
		}
		for _, port := range net.HTTP {
			var addr string
			if len(routes.NIC) > i {
				nic := routes.NIC[i]
				if proto, ok := nic.Protocol["http"]; ok {
					if pmap, ok := proto.Port[port]; ok {
						addr = pmap.Address
					}
				}
			}
			newNetwork.HTTP = append(newNetwork.HTTP, api.RouteMap{
				Port:    port,
				Address: addr,
			})
		}
		for _, port := range net.HTTPS {
			var addr string
			if len(routes.NIC) > i {
				nic := routes.NIC[i]
				if proto, ok := nic.Protocol["https"]; ok {
					if pmap, ok := proto.Port[port]; ok {
						addr = pmap.Address
					}
				}
			}
			newNetwork.HTTPS = append(newNetwork.HTTPS, api.RouteMap{
				Port:    port,
				Address: addr,
			})
		}
		apiNics = append(apiNics, newNetwork)
	}
	return apiNics
}