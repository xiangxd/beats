package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"regexp"
	"runtime"
	"time"

	"github.com/elastic/libbeat/cfgfile"
	"github.com/elastic/libbeat/common"
	"github.com/elastic/libbeat/logp"
	"github.com/elastic/libbeat/publisher"
	"github.com/elastic/libbeat/service"
)

// You can overwrite these, e.g.: go build -ldflags "-X main.Version 1.0.0-beta3"
var Version = "1.0.0-beta2"
var Name = "topbeat"

type ProcsMap map[int]*Process

type Topbeat struct {
	isAlive      bool
	period       time.Duration
	procs        []string
	procsMap     ProcsMap
	lastCpuTimes *CpuTimes

	events chan common.MapStr
}

func (t *Topbeat) MatchProcess(name string) bool {

	for _, reg := range t.procs {
		matched, _ := regexp.MatchString(reg, name)
		if matched {
			return true
		}
	}
	return false
}

func (t *Topbeat) getUsedMemPercent(m *MemStat) float64 {

	if m.Total == 0 {
		return 0.0
	}

	perc := float64(100*m.Used) / float64(m.Total)
	return Round(perc, .5, 2)
}

func (t *Topbeat) getRssPercent(m *ProcMemStat) float64 {

	mem_stat, err := GetMemory()
	if err != nil {
		logp.Warn("Getting memory details: %v", err)
		return 0.0
	}
	total_phymem := mem_stat.Total

	perc := (float64(m.Rss) / float64(total_phymem)) * 100

	return Round(perc, .5, 2)
}

func (t *Topbeat) getCpuPercentage(t2 *CpuTimes) float64 {

	t1 := t.lastCpuTimes

	perc := 0.0

	if t1 != nil {
		all_delta := t2.sum() - t1.sum()
		user_delta := t2.User - t1.User

		perc = float64(100*user_delta) / float64(all_delta)
	}
	t.lastCpuTimes = t2

	return Round(perc, .5, 2)
}

func (t *Topbeat) getProcCpuPercentage(proc *Process) float64 {

	oproc, ok := t.procsMap[proc.Pid]
	if ok {

		delta_proc := (proc.Cpu.User - oproc.Cpu.User) + (proc.Cpu.System - oproc.Cpu.System)
		delta_time := proc.ctime.Sub(oproc.ctime).Nanoseconds() / 1e6 // in milliseconds
		perc := float64(delta_proc) / float64(delta_time) * 100

		t.procsMap[proc.Pid] = proc

		return Round(perc, .5, 2)
	}
	return 0
}

func (t *Topbeat) Init(config TopConfig, events chan common.MapStr) error {

	if config.Period != nil {
		t.period = time.Duration(*config.Period) * time.Second
	} else {
		t.period = 1 * time.Second
	}
	if config.Procs != nil {
		t.procs = *config.Procs
	} else {
		t.procs = []string{".*"} //all processes
	}

	logp.Debug("topbeat", "Init toppbeat")
	logp.Debug("topbeat", "Follow processes %q\n", t.procs)
	logp.Debug("topbeat", "Period %v\n", t.period)
	t.events = events
	return nil
}

func (t *Topbeat) initProcStats() {

	t.procsMap = make(ProcsMap)

	if len(t.procs) == 0 {
		return
	}

	pids, err := Pids()
	if err != nil {
		logp.Warn("Getting the list of pids: %v", err)
	}

	logp.Debug("topbeat", "Pids: %v\n", pids)

	for _, pid := range pids {
		process, err := GetProcess(pid)
		if err != nil {
			logp.Debug("topbeat", "Skip process %d: %v", pid, err)
			continue
		}
		t.procsMap[process.Pid] = process
	}
}

func (t *Topbeat) exportProcStats() error {

	if len(t.procs) == 0 {
		return nil
	}

	pids, err := Pids()
	if err != nil {
		logp.Warn("Getting the list of pids: %v", err)
		return err
	}

	for _, pid := range pids {
		process, err := GetProcess(pid)
		if err != nil {
			logp.Debug("topbeat", "Skip process %d: %v", pid, err)
			continue
		}

		if t.MatchProcess(process.Name) {

			process.Cpu.UserPercent = t.getProcCpuPercentage(process)
			process.Mem.RssPercent = t.getRssPercent(&process.Mem)

			t.procsMap[process.Pid] = process

			event := common.MapStr{
				"timestamp":  common.Time(time.Now()),
				"type":       "proc",
				"proc.pid":   process.Pid,
				"proc.ppid":  process.Ppid,
				"proc.name":  process.Name,
				"proc.state": process.State,
				"proc.mem":   process.Mem,
				"proc.cpu":   process.Cpu,
			}
			t.events <- event
		}
	}
	return nil
}

func (t *Topbeat) exportSystemStats() error {

	load_stat, err := GetSystemLoad()
	if err != nil {
		logp.Warn("Getting load statistics: %v", err)
		return err
	}
	cpu_stat, err := GetCpuTimes()
	if err != nil {
		logp.Warn("Getting cpu times: %v", err)
		return err
	}

	cpu_stat.UserPercent = t.getCpuPercentage(cpu_stat)

	mem_stat, err := GetMemory()
	if err != nil {
		logp.Warn("Getting memory details: %v", err)
		return err
	}
	mem_stat.UsedPercent = t.getUsedMemPercent(mem_stat)

	swap_stat, err := GetSwap()
	if err != nil {
		logp.Warn("Getting swap details: %v", err)
		return err
	}
	// calculate swap usage in percent
	swap_stat.UsedPercent = t.getUsedMemPercent(swap_stat)

	event := common.MapStr{
		"timestamp": common.Time(time.Now()),
		"type":      "system",
		"load":      load_stat,
		"cpu":       cpu_stat,
		"mem":       mem_stat,
		"swap":      swap_stat,
	}

	t.events <- event

	return nil
}

func (t *Topbeat) procCpuPercent(proc *Process) float64 {

	oproc, ok := t.procsMap[proc.Pid]
	if ok {

		delta_proc := (proc.Cpu.User - oproc.Cpu.User) + (proc.Cpu.System - oproc.Cpu.System)
		delta_time := proc.ctime.Sub(oproc.ctime).Nanoseconds() / 1e6 // in milliseconds
		perc := (float64(delta_proc) / float64(delta_time) * 100) * float64(runtime.NumCPU())

		t.procsMap[proc.Pid] = proc

		return Round(perc, .5, 2)
	}
	return 0
}

func (t *Topbeat) Run() error {

	t.isAlive = true

	t.initProcStats()

	for t.isAlive {
		time.Sleep(t.period)

		t.exportSystemStats()
		t.exportProcStats()
	}
	return nil
}

func (t *Topbeat) Stop() {

	t.isAlive = false
}

func main() {

	// Use our own FlagSet, because some libraries pollute the global one
	var cmdLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	cfgfile.CmdLineFlags(cmdLine, Name)
	logp.CmdLineFlags(cmdLine)
	service.CmdLineFlags(cmdLine)

	publishDisabled := cmdLine.Bool("N", false, "Disable actual publishing for testing")
	printVersion := cmdLine.Bool("version", false, "Print version and exit")

	cmdLine.Parse(os.Args[1:])

	if *printVersion {
		fmt.Printf("%s version %s (%s)\n", Name, Version, runtime.GOARCH)
		return
	}

	err := cfgfile.Read(&Config)

	logp.Init(Name, &Config.Logging)

	logp.Debug("main", "Initializing output plugins")
	if err = publisher.Publisher.Init(*publishDisabled, Config.Output,
		Config.Shipper); err != nil {

		logp.Critical(err.Error())
		os.Exit(1)
	}

	topbeat := &Topbeat{}
	if err = topbeat.Init(Config.Input, publisher.Publisher.Queue); err != nil {
		logp.Critical(err.Error())
		os.Exit(1)
	}

	// Up to here was the initialization, now about running

	if cfgfile.IsTestConfig() {
		// all good, exit with 0
		os.Exit(0)
	}
	service.BeforeRun()

	service.HandleSignals(topbeat.Stop)

	// Startup successful, disable stderr logging if requested by
	// cmdline flag
	logp.SetStderr()

	logp.Debug("main", "Starting topbeat")

	err = topbeat.Run()
	if err != nil {
		logp.Critical("Sniffer main loop failed: %v", err)
		os.Exit(1)
	}

	logp.Debug("main", "Cleanup")
	service.Cleanup()
}

func Round(val float64, roundOn float64, places int) (newVal float64) {
	var round float64
	pow := math.Pow(10, float64(places))
	digit := pow * val
	_, div := math.Modf(digit)
	if div >= roundOn {
		round = math.Ceil(digit)
	} else {
		round = math.Floor(digit)
	}
	newVal = round / pow
	return
}
func (t *CpuTimes) sum() uint64 {

	return t.User + t.Nice + t.System + t.Idle + t.IOWait + t.Irq + t.SoftIrq + t.Steal
}
