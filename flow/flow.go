// Copyright 2017 Intel Corporation.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package flow provides functionality for constructing packet processing graph

// Preparations of construction:
// All construction should be between SystemInit and SystemStart functions.
// User command line options should be added as flags before SystemInit option - it will
// parse them as well as internal library options.

// Packet processing graph construction:
// NFF-GO library provides nine so-called Flow Functions for packet processing graph
// construction. They operate term "flow" however it is just abstraction for connecting
// them. Not anything beyond this. These nine flow functions are:
// Receive, Generate - for adding packets to graph
// Send, Stop - for removing packets from graph
// Handle - for handling packets inside graph
// Separate, Split, Count, Merge for combining flows inside graph
// All this functions can be added to the graph be "Set" functions like
// SetReceiver, SetSplitter, etc.

// Flow functions Generate, Handle, Separate and Split use user defined functions
// for processing. These functions are received each packet from flow (or new
// allocated packet in generate). Function types of user defined functions are
// also defined in this file.

// Package flow is the main package of NFF-GO library and should be always imported by
// user application.
package flow

import (
	"os"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/intel-go/nff-go/asm"
	"github.com/intel-go/nff-go/common"
	"github.com/intel-go/nff-go/low"
	"github.com/intel-go/nff-go/packet"
)

var openFlowsNumber = uint32(0)
var createdPorts []port
var portPair map[uint32](*port)
var schedState *scheduler
var vEach [10][burstSize]uint8

type Timer struct {
	t        *time.Ticker
	handler  func(UserContext)
	contexts []UserContext
	checks   []*bool
}

type processSegment struct {
	out      []low.Rings
	contexts []UserContext
	stype    uint8
}

// Flow is an abstraction for connecting flow functions with each other.
// Flow shouldn't be understood in any way beyond this.
type Flow struct {
	current       low.Rings
	segment       *processSegment
	previous      **Func
	inIndexNumber int32
}

type partitionCtx struct {
	currentAnswer       uint8
	currentCompare      uint64
	currentPacketNumber uint64
	N                   uint64
	M                   uint64
}

func (c partitionCtx) Copy() interface{} {
	return &partitionCtx{N: c.N, M: c.M, currentCompare: c.N}
}

func (c partitionCtx) Delete() {
}

type Func struct {
	sHandleFunction   HandleFunction
	sSeparateFunction SeparateFunction
	sSplitFunction    SplitFunction
	sFunc             func(*packet.Packet, *Func, UserContext) uint
	vHandleFunction   VectorHandleFunction
	vSeparateFunction VectorSeparateFunction
	vSplitFunction    VectorSplitFunction
	vFunc             func([]*packet.Packet, *[burstSize]bool, *[burstSize]uint8, *Func, UserContext)

	next            [](*Func)
	bufIndex        uint
	contextIndex    int
	followingNumber uint8
}

// GenerateFunction is a function type for user defined function which generates packets.
// Function receives preallocated packet where user should add
// its size and content.
type GenerateFunction func(*packet.Packet, UserContext)

// VectorGenerateFunction is a function type like GenerateFunction for vector generating
type VectorGenerateFunction func([]*packet.Packet, UserContext)

// HandleFunction is a function type for user defined function which handles packets.
// Function receives a packet from flow. User should parse it
// and make necessary changes. It is prohibit to free packet in this
// function.
type HandleFunction func(*packet.Packet, UserContext)

// VectorHandleFunction is a function type like HandleFunction for vector handling
type VectorHandleFunction func([]*packet.Packet, *[burstSize]bool, UserContext)

// SeparateFunction is a function type for user defined function which separates packets
// based on some rule for two flows. Functions receives a packet from flow.
// User should parse it and decide whether this packet should remains in
// this flow - return true, or should be sent to new added flow - return false.
type SeparateFunction func(*packet.Packet, UserContext) bool

// VectorSeparateFunction is a function type like SeparateFunction for vector separation
type VectorSeparateFunction func([]*packet.Packet, *[burstSize]bool, *[burstSize]bool, UserContext)

// SplitFunction is a function type for user defined function which splits packets
// based in some rule for multiple flows. Function receives a packet from
// flow. User should parse it and decide in which output flows this packet
// should be sent. Return number of flow shouldn't exceed target number
// which was put to SetSplitter function. Also it is assumed that "0"
// output flow is used for dropping packets - "Stop" function should be
// set after "Split" function in it.
type SplitFunction func(*packet.Packet, UserContext) uint

// VectorSplitFunction is a function type like SplitFunction for vector splitting
type VectorSplitFunction func([]*packet.Packet, *[burstSize]bool, *[burstSize]uint8, UserContext)

// Kni is a high level struct of KNI device. The device itself is stored
// in C memory in low.c and is defined by its port which is equal to port
// in this structure
type Kni struct {
	portId uint16
}

type receiveParameters struct {
	out  low.Rings
	port *low.Port
	kni  bool
}

func addReceiver(portId uint16, kni bool, out low.Rings, inIndexNumber int32) {
	par := new(receiveParameters)
	par.port = low.GetPort(portId)
	par.out = out
	par.kni = kni
	if kni {
		schedState.addFF("KNI receiver", nil, recvKNI, nil, par, nil, sendReceiveKNI, 0)
	} else {
		schedState.addFF("receiver", nil, recvRSS, nil, par, nil, receiveRSS, inIndexNumber)
	}
}

type generateParameters struct {
	out                    low.Rings
	generateFunction       GenerateFunction
	vectorGenerateFunction VectorGenerateFunction
	mempool                *low.Mempool
	targetSpeed            float64
}

func addGenerator(out low.Rings, generateFunction GenerateFunction, context UserContext) {
	par := new(generateParameters)
	par.out = out
	par.generateFunction = generateFunction
	ctx := make([]UserContext, 1, 1)
	ctx[0] = context
	schedState.addFF("generator", nil, nil, pGenerate, par, &ctx, generate, 0)
}

func addFastGenerator(out low.Rings, generateFunction GenerateFunction,
	vectorGenerateFunction VectorGenerateFunction, targetSpeed uint64, context UserContext) error {
	fTargetSpeed := float64(targetSpeed)
	if fTargetSpeed <= 0 {
		return common.WrapWithNFError(nil, "Target speed value should be > 0", common.BadArgument)
	} else if fTargetSpeed/(1000 /*milleseconds*/ /float64(schedTime)) < float64(burstSize) {
		// TargetSpeed per schedTime should be more than burstSize because one burstSize packets in
		// one schedTime seconds are out minimal scheduling part. We can't make generate speed less than this.
		return common.WrapWithNFError(nil, "Target speed per schedTime should be more than burstSize", common.BadArgument)
	}
	par := new(generateParameters)
	par.out = out
	par.generateFunction = generateFunction
	par.mempool = low.CreateMempool("fast generate")
	par.vectorGenerateFunction = vectorGenerateFunction
	par.targetSpeed = fTargetSpeed
	ctx := make([]UserContext, 1, 1)
	ctx[0] = context
	schedState.addFF("fast generator", nil, nil, pFastGenerate, par, &ctx, fastGenerate, 0)
	return nil
}

type sendParameters struct {
	in    low.Rings
	queue int16
	port  uint16
}

func addSender(port uint16, queue int16, in low.Rings, inIndexNumber int32) {
	par := new(sendParameters)
	par.port = port
	par.queue = queue
	par.in = in
	if queue != -1 {
		schedState.addFF("sender", nil, send, nil, par, nil, sendReceiveKNI, inIndexNumber)
	} else {
		schedState.addFF("KNI sender", nil, send, nil, par, nil, sendReceiveKNI, inIndexNumber)
	}
}

type copyParameters struct {
	in      low.Rings
	out     low.Rings
	outCopy low.Rings
	mempool *low.Mempool
}

func addCopier(in low.Rings, out low.Rings, outCopy low.Rings, inIndexNumber int32) {
	par := new(copyParameters)
	par.in = in
	par.out = out
	par.outCopy = outCopy
	par.mempool = low.CreateMempool("copy")
	schedState.addFF("copy", nil, nil, pcopy, par, nil, segmentCopy, inIndexNumber)
}

func makePartitioner(N uint64, M uint64) *Func {
	f := new(Func)
	f.sFunc = partition
	f.vFunc = vPartition
	f.next = make([]*Func, 2, 2)
	f.followingNumber = 2
	return f
}

func makeSeparator(separateFunction SeparateFunction, vectorSeparateFunction VectorSeparateFunction) *Func {
	f := new(Func)
	f.sSeparateFunction = separateFunction
	f.vSeparateFunction = vectorSeparateFunction
	f.sFunc = separate
	f.vFunc = vSeparate
	f.next = make([]*Func, 2, 2)
	f.followingNumber = 2
	return f
}

func makeSplitter(splitFunction SplitFunction, vectorSplitFunction VectorSplitFunction, n uint8) *Func {
	f := new(Func)
	f.sSplitFunction = splitFunction
	f.vSplitFunction = vectorSplitFunction
	f.sFunc = split
	f.vFunc = vSplit
	f.next = make([]*Func, n, n)
	f.followingNumber = n
	return f
}

func makeHandler(handleFunction HandleFunction, vectorHandleFunction VectorHandleFunction) *Func {
	f := new(Func)
	f.sHandleFunction = handleFunction
	f.vHandleFunction = vectorHandleFunction
	f.sFunc = handle
	f.vFunc = vHandle
	f.next = make([]*Func, 1, 1)
	f.followingNumber = 1
	return f
}

type writeParameters struct {
	in       low.Rings
	filename string
}

func addWriter(filename string, in low.Rings, inIndexNumber int32) {
	par := new(writeParameters)
	par.in = in
	par.filename = filename
	schedState.addFF("writer", write, nil, nil, par, nil, readWrite, inIndexNumber)
}

type readParameters struct {
	out      low.Rings
	filename string
	repcount int32
}

func addReader(filename string, out low.Rings, repcount int32) {
	par := new(readParameters)
	par.out = out
	par.filename = filename
	par.repcount = repcount
	schedState.addFF("reader", read, nil, nil, par, nil, readWrite, 0)
}

func makeSlice(out low.Rings, segment *processSegment) *Func {
	f := new(Func)
	f.sFunc = constructSlice
	f.vFunc = vConstructSlice
	segment.out = append(segment.out, out)
	f.bufIndex = uint(len(segment.out) - 1)
	f.followingNumber = 0
	return f
}

type segmentParameters struct {
	in        low.Rings
	out       *([]low.Rings)
	firstFunc *Func
	stype     *uint8
}

func addSegment(in low.Rings, first *Func, inIndexNumber int32) *processSegment {
	par := new(segmentParameters)
	par.in = in
	par.firstFunc = first
	segment := new(processSegment)
	segment.out = make([]low.Rings, 0, 0)
	segment.contexts = make([](UserContext), 0, 0)
	par.out = &segment.out
	par.stype = &segment.stype
	schedState.addFF("segment", nil, nil, segmentProcess, par, &segment.contexts, segmentCopy, inIndexNumber)
	return segment
}

type HWCapability int

const (
	HWTXChecksumCapability HWCapability = iota
)

// CheckHWCapability return true if hardware offloading capability
// present in all ports. Otherwise it returns false.
func CheckHWCapability(capa HWCapability, ports []uint16) bool {
	for p := range ports {
		switch capa {
		case HWTXChecksumCapability:
			if !low.CheckHWTXChecksumCapability(ports[p]) {
				return false
			}
		}
	}
	return true
}

// SetUseHWCapability enables or disables using a hardware offloading
// capability.
func SetUseHWCapability(capa HWCapability, use bool) {
	switch capa {
	case HWTXChecksumCapability:
		packet.SetHWTXChecksumFlag(use)
	}
}

const burstSize = 32
const reportMbits = false

var sizeMultiplier uint
var schedTime uint
var hwtxchecksum bool

type port struct {
	wasRequested   bool // has user requested any send/receive operations at this port
	txQueuesNumber int16
	willReceive    bool // will this port receive packets
	willKNI        bool // will this port has assigned KNI device
	KNICoreIndex   int
	port           uint16
	MAC            [common.EtherAddrLen]uint8
	InIndex        int32
}

// Config is a struct with all parameters, which user can pass to NFF-GO library
type Config struct {
	// Specifies cores which will be available for scheduler to place
	// flow functions and their clones.
	CPUList string
	// If true, scheduler is disabled entirely. Default value is false.
	DisableScheduler bool
	// If true, scheduler does not stop any previously cloned flow
	// function threads. Default value is false.
	PersistentClones bool
	// If true, Stop routine gets a dedicated CPU core instead of
	// running together with scheduler. Default value is false.
	StopOnDedicatedCore bool
	// Calculate IPv4, UDP and TCP checksums in hardware. This flag
	// slows down general TX processing, so it should be enabled if
	// applications intends to modify packets often, and therefore
	// needs to recalculate their checksums. If application doesn't
	// modify many packets, it may chose to calculate checksums in SW
	// and leave this flag off. Default value is false.
	HWTXChecksum bool
	// Specifies number of mbufs in mempool per port. Default value is
	// 8191.
	MbufNumber uint
	// Specifies number of mbufs in per-CPU core cache in
	// mempool. Default value is 250.
	MbufCacheSize uint
	// Number of burstSize groups in all rings. This should be power
	// of 2. Default value is 256.
	RingSize uint
	// Time between scheduler actions in miliseconds. Default value is
	// 1500.
	ScaleTime uint
	// Time in miliseconds for scheduler to check changing of flow
	// function behaviour. Default value is 10000.
	CheckTime uint
	// Time in miliseconds for scheduler to display statistics.
	// Default value is 1000.
	DebugTime uint
	// Specifies logging type. Default value is common.No |
	// common.Initialization | common.Debug.
	LogType common.LogType
	// Command line arguments to pass to DPDK initialization.
	DPDKArgs []string
	// Is user going to use KNI
	NeedKNI bool
	// Maximum simultaneous receives that should handle all
	// input at your network card
	MaxRecv int
	// Limits parallel instances. 1 for one instance, 1000 for RSS count determine instances
	MaxInIndex int32
	// Scheduler should clone functions even if ti can lead to reordering.
	// This option should be switch off for all high level reassembling like TCP or HTTP
	RestrictedCloning bool
}

// SystemInit is initialization of system. This function should be always called before graph construction.
func SystemInit(args *Config) error {
	if args == nil {
		args = &Config{}
	}
	CPUCoresNumber := runtime.NumCPU()
	var cpus []int
	var err error
	if args.CPUList != "" {
		if cpus, err = common.HandleCPUList(args.CPUList, CPUCoresNumber); err != nil {
			return err
		}
	} else {
		cpus = common.GetDefaultCPUs(CPUCoresNumber)
	}

	schedulerOff := args.DisableScheduler
	schedulerOffRemove := args.PersistentClones
	stopDedicatedCore := args.StopOnDedicatedCore
	hwtxchecksum = args.HWTXChecksum
	anyway := !args.RestrictedCloning

	mbufNumber := uint(8191)
	if args.MbufNumber != 0 {
		mbufNumber = args.MbufNumber
	}

	mbufCacheSize := uint(250)
	if args.MbufCacheSize != 0 {
		mbufCacheSize = args.MbufCacheSize
	}

	sizeMultiplier = 64
	if args.RingSize != 0 {
		sizeMultiplier = args.RingSize
	}

	schedTime = 500
	if args.ScaleTime != 0 {
		schedTime = args.ScaleTime
	}

	checkTime := uint(10000)
	if args.CheckTime != 0 {
		checkTime = args.CheckTime
	}

	debugTime := uint(1000)
	if args.DebugTime != 0 {
		debugTime = args.DebugTime
	}

	if debugTime < schedTime {
		return common.WrapWithNFError(nil, "debugTime should be larger or equal to schedTime", common.Fail)
	}

	needKNI := 0
	if args.NeedKNI != false {
		needKNI = 1
	}

	logType := common.No | common.Initialization | common.Debug
	if args.LogType != 0 {
		logType = args.LogType
	}
	common.SetLogType(logType)

	maxRecv := 2
	if args.MaxRecv != 0 {
		needKNI = args.MaxRecv
	}

	maxInIndex := int32(16)
	if schedulerOff == true {
		maxInIndex = 1
	}
	if args.MaxInIndex != 0 {
		maxInIndex = args.MaxInIndex
	}

	argc, argv := low.InitDPDKArguments(args.DPDKArgs)
	// We want to add new clone if input ring is approximately 80% full
	maxPacketsToClone := uint32(sizeMultiplier * burstSize / 5 * 4)
	// TODO all low level initialization here! Now everything is default.
	// Init eal
	common.LogTitle(common.Initialization, "------------***-------- Initializing DPDK --------***------------")
	if err := low.InitDPDK(argc, argv, burstSize, mbufNumber, mbufCacheSize, needKNI); err != nil {
		return err
	}
	// Init Ports
	createdPorts = make([]port, low.GetPortsNumber(), low.GetPortsNumber())
	for i := range createdPorts {
		createdPorts[i].port = uint16(i)
		if maxInIndex > low.CheckPortRSS(createdPorts[i].port) {
			createdPorts[i].InIndex = low.CheckPortRSS(createdPorts[i].port)
		} else {
			createdPorts[i].InIndex = maxInIndex
		}
	}
	portPair = make(map[uint32](*port))
	// Init scheduler
	common.LogTitle(common.Initialization, "------------***------ Initializing scheduler -----***------------")
	StopRing := low.CreateRings(burstSize*sizeMultiplier, maxInIndex)
	common.LogDebug(common.Initialization, "Scheduler can use cores:", cpus)
	schedState = newScheduler(cpus, schedulerOff, schedulerOffRemove, stopDedicatedCore, StopRing, checkTime, debugTime, maxPacketsToClone, maxRecv, anyway)
	// Init packet processing
	packet.SetHWTXChecksumFlag(hwtxchecksum)
	for i := 0; i < 10; i++ {
		for j := 0; j < burstSize; j++ {
			vEach[i][j] = uint8(i)
		}
	}
	return nil
}

// SystemInitPortsAndMemory performs all initialization necessary to
// create and send new packets before scheduler may be started.
func SystemInitPortsAndMemory() error {
	if openFlowsNumber != 0 {
		return common.WrapWithNFError(nil, "Some flows are left open at the end of configuration!", common.OpenedFlowAtTheEnd)
	}
	common.LogTitle(common.Initialization, "------------***---------- Creating ports ---------***------------")
	for i := range createdPorts {
		if createdPorts[i].wasRequested {
			if err := low.CreatePort(createdPorts[i].port, createdPorts[i].willReceive,
				uint16(createdPorts[i].txQueuesNumber), true, hwtxchecksum, createdPorts[i].InIndex); err != nil {
				return err
			}
		}
		createdPorts[i].MAC = GetPortMACAddress(createdPorts[i].port)
		common.LogDebug(common.Initialization, "Port", createdPorts[i].port, "MAC address:", packet.MACToString(createdPorts[i].MAC))
	}
	common.LogTitle(common.Initialization, "------------***------ Starting FlowFunctions -----***------------")
	// Init low performance mempool
	packet.SetNonPerfMempool(low.CreateMempool("slow operations"))
	return nil
}

// SystemStartScheduler starts scheduler packet processing. Function
// does not return.
func SystemStartScheduler() error {
	if err := schedState.systemStart(); err != nil {
		return common.WrapWithNFError(err, "scheduler start failed", common.Fail)
	}
	common.LogTitle(common.Initialization, "------------***---------- NFF-GO Started ---------***------------")
	schedState.schedule(schedTime)
	return nil
}

// SystemStart starts system - begin packet receiving and packet sending.
// This functions should be always called after flow graph construction.
// Function can panic during execution.
func SystemStart() error {
	err := SystemInitPortsAndMemory()
	if err != nil {
		return err
	}
	err = SystemStartScheduler()
	if err != nil {
		return err
	}
	return nil
}

// SystemStop stops the system. All Flow functions plus resource releasing
// Doesn't cleanup DPDK
func SystemStop() error {
	// TODO we should release rings here
	schedState.systemStop()
	for i := range createdPorts {
		if createdPorts[i].wasRequested {
			low.StopPort(createdPorts[i].port)
			createdPorts[i].wasRequested = false
			createdPorts[i].txQueuesNumber = 0
			createdPorts[i].willReceive = false
		}
		if createdPorts[i].willKNI {
			err := low.FreeKNI(createdPorts[i].port)
			if err != nil {
				return err
			}
			schedState.setCoreByIndex(createdPorts[i].KNICoreIndex)
			createdPorts[i].willKNI = false
		}
	}
	low.FreeMempools()
	return nil
}

// SystemReset stops whole framework plus cleanup DPDK
// TODO DPDK cleanup is now incomplete at DPDK side
// It is n't able to re-init after it and also is
// under deprecated pragma. Need to fix after DPDK changes.
func SystemReset() {
	SystemStop()
	low.StopDPDK()
}

// SetSenderFile adds write function to flow graph.
// Gets flow which packets will be written to file and
// target file name.
func SetSenderFile(IN *Flow, filename string) error {
	if err := checkFlow(IN); err != nil {
		return err
	}
	addWriter(filename, finishFlow(IN), IN.inIndexNumber)
	return nil
}

// SetReceiverFile adds read function to flow graph.
// Gets name of pcap formatted file and number of reads. If repcount = -1,
// file is read infinitely in circle.
// Returns new opened flow with read packets.
func SetReceiverFile(filename string, repcount int32) (OUT *Flow) {
	rings := low.CreateRings(burstSize*sizeMultiplier, 1)
	addReader(filename, rings, repcount)
	return newFlow(rings, 1)
}

// SetReceiver adds receive function to flow graph.
// Gets port number from which packets will be received.
// Receive queue will be added to port automatically.
// Returns new opened flow with received packets
func SetReceiver(portId uint16) (OUT *Flow, err error) {
	if portId >= uint16(len(createdPorts)) {
		return nil, common.WrapWithNFError(nil, "Requested receive port exceeds number of ports which can be used by DPDK (bind to DPDK).", common.ReqTooManyPorts)
	}
	if createdPorts[portId].willReceive {
		return nil, common.WrapWithNFError(nil, "Requested receive port was already set to receive. Two receives from one port are prohibited.", common.MultipleReceivePort)
	}
	createdPorts[portId].wasRequested = true
	createdPorts[portId].willReceive = true
	rings := low.CreateRings(burstSize*sizeMultiplier, createdPorts[portId].InIndex)
	addReceiver(portId, false, rings, createdPorts[portId].InIndex)
	return newFlow(rings, createdPorts[portId].InIndex), nil
}

// SetReceiverKNI adds function receive from KNI to flow graph.
// Gets KNI device from which packets will be received.
// Receive queue will be added to port automatically.
// Returns new opened flow with received packets
func SetReceiverKNI(kni *Kni) (OUT *Flow) {
	rings := low.CreateRings(burstSize*sizeMultiplier, 1)
	addReceiver(kni.portId, true, rings, 1)
	return newFlow(rings, 1)
}

// SetFastGenerator adds clonable generate function to flow graph.
// Gets user-defined generate function, target speed of generation user wants to achieve and context.
// Returns new open flow with generated packets.
// Function tries to achieve target speed by cloning.
func SetFastGenerator(f GenerateFunction, targetSpeed uint64, context UserContext) (OUT *Flow, err error) {
	rings := low.CreateRings(burstSize*sizeMultiplier, 1)
	if err := addFastGenerator(rings, f, nil, targetSpeed, context); err != nil {
		return nil, err
	}
	return newFlow(rings, 1), nil
}

// SetVectorFastGenerator adds clonable vector generate function to flow graph.
// Gets user-defined vector generate function, target speed of generation user wants to achieve and context.
// Returns new open flow with generated packets.
// Function tries to achieve target speed by cloning.
func SetVectorFastGenerator(f VectorGenerateFunction, targetSpeed uint64, context UserContext) (OUT *Flow, err error) {
	rings := low.CreateRings(burstSize*sizeMultiplier, 1)
	if err := addFastGenerator(rings, nil, f, targetSpeed, context); err != nil {
		return nil, err
	}
	return newFlow(rings, 1), nil
}

// SetGenerator adds non-clonable generate flow function to flow graph.
// Gets user-defined generate function and context.
// Returns new open flow with generated packets.
// Single packet non-clonable flow function will be added. It can be used for waiting of
// input user packets.
func SetGenerator(f GenerateFunction, context UserContext) (OUT *Flow) {
	rings := low.CreateRings(burstSize*sizeMultiplier, 1)
	addGenerator(rings, f, context)
	return newFlow(rings, 1)
}

// SetSender adds send function to flow graph.
// Gets flow which will be closed and its packets will be send and port number for which packets will be sent.
// Send queue will be added to port automatically.
func SetSender(IN *Flow, portId uint16) error {
	if err := checkFlow(IN); err != nil {
		return err
	}
	if portId >= uint16(len(createdPorts)) {
		return common.WrapWithNFError(nil, "Requested send port exceeds number of ports which can be used by DPDK (bind to DPDK).", common.ReqTooManyPorts)
	}
	createdPorts[portId].wasRequested = true
	addSender(portId, createdPorts[portId].txQueuesNumber, finishFlow(IN), IN.inIndexNumber)
	createdPorts[portId].txQueuesNumber++
	return nil
}

// SetSenderKNI adds function sending to KNI to flow graph.
// Gets flow which will be closed and its packets will be send to given KNI device.
// Send queue will be added to port automatically.
func SetSenderKNI(IN *Flow, kni *Kni) error {
	if err := checkFlow(IN); err != nil {
		return err
	}
	addSender(kni.portId, -1, finishFlow(IN), IN.inIndexNumber)
	return nil
}

// SetCopier adds copy function to flow graph.
// Gets flow which will be copied.
func SetCopier(IN *Flow) (OUT *Flow, err error) {
	if err := checkFlow(IN); err != nil {
		return nil, err
	}
	ringFirst := low.CreateRings(burstSize*sizeMultiplier, IN.inIndexNumber)
	ringSecond := low.CreateRings(burstSize*sizeMultiplier, IN.inIndexNumber)
	if IN.segment == nil {
		addCopier(IN.current, ringFirst, ringSecond, IN.inIndexNumber)
	} else {
		tRing := low.CreateRings(burstSize*sizeMultiplier, IN.inIndexNumber)
		ms := makeSlice(tRing, IN.segment)
		segmentInsert(IN, ms, false, nil, 0, 0)
		addCopier(tRing, ringFirst, ringSecond, IN.inIndexNumber)
		IN.segment = nil
	}
	IN.current = ringFirst
	return newFlow(ringSecond, IN.inIndexNumber), nil
}

// SetPartitioner adds partition function to flow graph.
// Gets input flow and N and M constants. Returns new opened flow.
// Each loop N packets will be remained in input flow, next M packets will be sent to new flow.
// It is advised not to use this function less then (75, 75) for performance reasons.
// We make partition function unclonable. The most complex task is (1,1).
// It means that if you would like to simply divide a flow
// it is recommended to use (75,75) instead of (1,1) for performance reasons.
func SetPartitioner(IN *Flow, N uint64, M uint64) (OUT *Flow, err error) {
	if N == 0 || M == 0 {
		common.LogWarning(common.Initialization, "One of SetPartitioner function's arguments is zero.")
	}
	partition := makePartitioner(N, M)
	ctx := new(partitionCtx)
	ctx.N = N
	ctx.M = M
	if err := segmentInsert(IN, partition, false, *ctx, 0, 0); err != nil {
		return nil, err
	}
	return newFlowSegment(IN.segment, &partition.next[1], IN.inIndexNumber), nil
}

// SetSeparator adds separate function to flow graph.
// Gets flow, user defined separate function and context. Returns new opened flow.
// Each packet from input flow will be remain inside input packet if
// user defined function returns "true" and is sent to new flow otherwise.
func SetSeparator(IN *Flow, separateFunction SeparateFunction, context UserContext) (OUT *Flow, err error) {
	separate := makeSeparator(separateFunction, nil)
	if err := segmentInsert(IN, separate, false, context, 1, 1); err != nil {
		return nil, err
	}
	return newFlowSegment(IN.segment, &separate.next[0], IN.inIndexNumber), nil
}

// SetVectorSeparator adds vector separate function to flow graph.
// Gets flow, user defined vector separate function and context. Returns new opened flow.
// Each packet from input flow will be remain inside input packet if
// user defined function returns "true" and is sent to new flow otherwise.
func SetVectorSeparator(IN *Flow, vectorSeparateFunction VectorSeparateFunction, context UserContext) (OUT *Flow, err error) {
	separate := makeSeparator(nil, vectorSeparateFunction)
	if err := segmentInsert(IN, separate, false, context, 2, 1); err != nil {
		return nil, err
	}
	return newFlowSegment(IN.segment, &separate.next[0], IN.inIndexNumber), nil
}

// SetSplitter adds split function to flow graph.
// Gets flow, user defined split function, flowNumber of new flows and context.
// Returns array of new opened flows with corresponding length.
// Each packet from input flow will be sent to one of new flows based on
// user defined function output for this packet.
func SetSplitter(IN *Flow, splitFunction SplitFunction, flowNumber uint, context UserContext) (OutArray [](*Flow), err error) {
	if err := checkFlow(IN); err != nil {
		return nil, err
	}
	split := makeSplitter(splitFunction, nil, uint8(flowNumber))
	segmentInsert(IN, split, true, context, 1, 0)
	OutArray = make([](*Flow), flowNumber, flowNumber)
	for i := range OutArray {
		OutArray[i] = newFlowSegment(IN.segment, &split.next[i], IN.inIndexNumber)
	}
	return OutArray, nil
}

// SetVectorSplitter adds vector split function to flow graph.
// Gets flow, user defined vector split function, flowNumber of new flows and context.
// Returns array of new opened flows with corresponding length.
// Each packet from input flow will be sent to one of new flows based on
// user defined function output for this packet.
func SetVectorSplitter(IN *Flow, vectorSplitFunction VectorSplitFunction, flowNumber uint, context UserContext) (OutArray [](*Flow), err error) {
	if err := checkFlow(IN); err != nil {
		return nil, err
	}
	split := makeSplitter(nil, vectorSplitFunction, uint8(flowNumber))
	segmentInsert(IN, split, true, context, 2, 0)
	OutArray = make([](*Flow), flowNumber, flowNumber)
	for i := range OutArray {
		OutArray[i] = newFlowSegment(IN.segment, &split.next[i], IN.inIndexNumber)
	}
	return OutArray, nil
}

// SetStopper adds stop function to flow graph.
// Gets flow which will be closed and all packets from each will be dropped.
func SetStopper(IN *Flow) error {
	if err := checkFlow(IN); err != nil {
		return err
	}
	if IN.segment == nil {
		merge(IN.current, schedState.StopRing)
		closeFlow(IN)
	} else {
		ms := makeSlice(schedState.StopRing, IN.segment)
		segmentInsert(IN, ms, true, nil, 0, 0)
	}
	return nil
}

// DealARPICMP is predefined function which will generate
// replies to ARP and ICMP requests and automatically extract
// corresponding packets from input flow.
// If used after merge, function answers packets received on all input ports.
func DealARPICMP(IN *Flow) error {
	return SetHandlerDrop(IN, handleARPICMPRequests, nil)
}

// SetHandler adds handle function to flow graph.
// Gets flow, user defined handle function and context.
// Each packet from input flow will be handle inside user defined function
// and sent further in the same flow.
func SetHandler(IN *Flow, handleFunction HandleFunction, context UserContext) error {
	handle := makeHandler(handleFunction, nil)
	return segmentInsert(IN, handle, false, context, 1, 0)
}

// SetVectorHandler adds vector handle function to flow graph.
// Gets flow, user defined vector handle function and context.
// Each packet from input flow will be handle inside user defined function
// and sent further in the same flow.
func SetVectorHandler(IN *Flow, vectorHandleFunction VectorHandleFunction, context UserContext) error {
	handle := makeHandler(nil, vectorHandleFunction)
	return segmentInsert(IN, handle, false, context, 2, 0)
}

// SetHandlerDrop adds vector handle function to flow graph.
// Gets flow, user defined handle function and context.
// User defined function can return boolean value.
// If user function returns false after handling a packet it is dropped automatically.
func SetHandlerDrop(IN *Flow, separateFunction SeparateFunction, context UserContext) error {
	separate := makeSeparator(separateFunction, nil)
	if err := segmentInsert(IN, separate, false, context, 1, 1); err != nil {
		return err
	}
	return SetStopper(newFlowSegment(IN.segment, &separate.next[0], IN.inIndexNumber))
}

// SetVectorHandlerDrop adds vector handle function to flow graph.
// Gets flow, user defined vector handle function and context.
// User defined function can return boolean value.
// If user function returns false after handling a packet it is dropped automatically.
func SetVectorHandlerDrop(IN *Flow, vectorSeparateFunction VectorSeparateFunction, context UserContext) error {
	separate := makeSeparator(nil, vectorSeparateFunction)
	if err := segmentInsert(IN, separate, false, context, 2, 1); err != nil {
		return err
	}
	return SetStopper(newFlowSegment(IN.segment, &separate.next[0], IN.inIndexNumber))
}

// SetMerger adds merge function to flow graph.
// Gets any number of flows. Returns new opened flow.
// All input flows will be closed. All packets from all these flows will be sent to new flow.
// This function isn't use any cores. It changes output flows of other functions at initialization stage.
func SetMerger(InArray ...*Flow) (OUT *Flow, err error) {
	max := int32(0)
	for i := range InArray {
		if InArray[i].inIndexNumber > max {
			max = InArray[i].inIndexNumber
		}
	}
	rings := low.CreateRings(burstSize*sizeMultiplier, max)
	for i := range InArray {
		if err := checkFlow(InArray[i]); err != nil {
			return nil, err
		}
		if InArray[i].segment == nil {
			merge(InArray[i].current, rings)
			closeFlow(InArray[i])
		} else {
			// TODO merge finishes segment even if this is merge inside it. Need to optimize.
			ms := makeSlice(rings, InArray[i].segment)
			segmentInsert(InArray[i], ms, true, nil, 0, 0)
		}
	}
	return newFlow(rings, max), nil
}

// GetPortMACAddress returns default MAC address of an Ethernet port.
func GetPortMACAddress(port uint16) [common.EtherAddrLen]uint8 {
	return low.GetPortMACAddress(port)
}

// SetIPForPort sets IP for specified port if it was created. Not thread safe.
// Return error if requested port isn't exist or wasn't previously requested.
func SetIPForPort(port uint16, ip uint32) error {
	for i := range createdPorts {
		if createdPorts[i].port == port && createdPorts[i].wasRequested {
			portPair[ip] = &createdPorts[i]
			return nil
		}
	}
	return common.WrapWithNFError(nil, "Port number in wrong or port was not requested", common.WrongPort)
}

// Service functions for Flow
func newFlow(rings low.Rings, inIndexNumber int32) *Flow {
	OUT := new(Flow)
	OUT.current = rings
	OUT.inIndexNumber = inIndexNumber
	openFlowsNumber++
	return OUT
}

func newFlowSegment(segment *processSegment, previous **Func, inIndexNumber int32) *Flow {
	OUT := newFlow(nil, inIndexNumber)
	OUT.segment = segment
	OUT.previous = previous
	return OUT
}

func finishFlow(IN *Flow) low.Rings {
	var ring low.Rings
	if IN.segment == nil {
		ring = IN.current
		closeFlow(IN)
	} else {
		ring = low.CreateRings(burstSize*sizeMultiplier, IN.inIndexNumber)
		ms := makeSlice(ring, IN.segment)
		segmentInsert(IN, ms, true, nil, 0, 0)
	}
	return ring
}

func closeFlow(IN *Flow) {
	IN.current = nil
	IN.previous = nil
	openFlowsNumber--
}

func segmentInsert(IN *Flow, f *Func, willClose bool, context UserContext, setType uint8, nextBranch uint8) error {
	if err := checkFlow(IN); err != nil {
		return err
	}
	if IN.segment == nil {
		IN.segment = addSegment(IN.current, f, IN.inIndexNumber)
		IN.segment.stype = setType
	} else {
		if setType > 0 && IN.segment.stype > 0 && setType != IN.segment.stype {
			// Try to combine scalar and vector code. Start new segment
			ring := low.CreateRings(burstSize*sizeMultiplier, IN.inIndexNumber)
			ms := makeSlice(ring, IN.segment)
			segmentInsert(IN, ms, false, nil, 0, 0)
			IN.segment = nil
			IN.current = ring
			segmentInsert(IN, f, willClose, context, setType, nextBranch)
			return nil
		}
		if setType > 0 && IN.segment.stype == 0 {
			// Current segment is universal. Set new scalar/vector type to it
			IN.segment.stype = setType
		}
		*IN.previous = f
	}
	if willClose {
		closeFlow(IN)
	} else if f.next != nil {
		IN.previous = &f.next[nextBranch]
	}
	IN.segment.contexts = append(IN.segment.contexts, context)
	f.contextIndex = len(IN.segment.contexts) - 1
	return nil
}

func segmentProcess(parameters interface{}, inIndex []int32, stopper [2]chan int, report chan reportPair, context []UserContext) {
	// For scalar and vector parts
	lp := parameters.(*segmentParameters)
	IN := lp.in
	OUT := *lp.out
	scalar := (*lp.stype != 2)
	outNumber := len(*lp.out)
	InputMbufs := make([]uintptr, burstSize, burstSize)
	OutputMbufs := make([][]uintptr, outNumber)
	countOfPackets := make([]int, outNumber)
	for index := range OutputMbufs {
		OutputMbufs[index] = make([]uintptr, burstSize)
		countOfPackets[index] = 0
	}
	var currentState reportPair
	var pause int
	firstFunc := lp.firstFunc
	// For scalar part
	var tempPacket *packet.Packet
	// For vector part
	tempPackets := make([]*packet.Packet, burstSize)
	type pair struct {
		f    *Func
		mask [burstSize]bool
	}
	def := make([]pair, 30, 30)
	var currentMask [burstSize]bool
	var answers [burstSize]uint8
	tick := time.NewTicker(time.Duration(schedTime) * time.Millisecond)
	stopper[1] <- 2 // Answer that function is ready

	for {
		select {
		case pause = <-stopper[0]:
			tick.Stop()
			if pause == -1 {
				// It is time to close this clone
				for i := range context {
					if context[i] != nil {
						context[i].Delete()
					}
				}
				stopper[1] <- 1
				return
			} else {
				// For any events with this function we should restart timer
				// We don't do it regularly without any events due to performance
				tick = time.NewTicker(time.Duration(schedTime) * time.Millisecond)
				currentState = reportPair{}
			}
		case <-tick.C:
			report <- currentState
			currentState = reportPair{}
		default:
			for q := int32(1); q < inIndex[0]+1; q++ {
				n := IN[inIndex[q]].DequeueBurst(InputMbufs, burstSize)
				if n == 0 {
					// GO parks goroutines while Sleep. So Sleep lasts more time than our precision
					// we just want to slow goroutine down without parking, so loop is OK for this.
					// time.Now lasts approximately 70ns and this satisfies us
					if pause != 0 {
						// pause should be non 0 only if function works with ONE inIndex
						a := time.Now()
						for time.Since(a) < time.Duration(pause*int(burstSize))*time.Nanosecond {
						}
					}
					currentState.ZeroAttempts[q-1]++
					continue
				}
				if scalar { // Scalar code
					for i := uint(0); i < n; i++ {
						currentFunc := firstFunc
						tempPacket = packet.ExtractPacket(InputMbufs[i])
						for {
							nextIndex := currentFunc.sFunc(tempPacket, currentFunc, context[currentFunc.contextIndex])
							if currentFunc.followingNumber == 0 {
								// We have constructSlice -> put packets to output slices
								OutputMbufs[nextIndex][countOfPackets[nextIndex]] = InputMbufs[i]
								countOfPackets[nextIndex]++
								if reportMbits {
									currentState.V.Bytes += uint64(tempPacket.GetPacketLen())
								}
								break
							}
							currentFunc = currentFunc.next[nextIndex]
						}
					}
					for index := 0; index < outNumber; index++ {
						if countOfPackets[index] == 0 {
							continue
						}
						safeEnqueue(OUT[index][inIndex[q]], OutputMbufs[index], uint(countOfPackets[index]))
						currentState.V.Packets += uint64(countOfPackets[index])
						countOfPackets[index] = 0
					}
				} else { // Vector code
					packet.ExtractPackets(tempPackets, InputMbufs, n)
					def[0].f = firstFunc
					for i := uint(0); i < burstSize; i++ {
						def[0].mask[i] = (i < n)
					}
					st := 0
					for st != -1 {
						cur := def[st].f
						cur.vFunc(tempPackets, &def[st].mask, &answers, cur, context[cur.contextIndex])
						if cur.followingNumber == 0 {
							// We have constructSlice -> put packets inside ring, it is an end of segment
							count := FillSliceFromMask(InputMbufs, &def[st].mask, OutputMbufs[0])
							safeEnqueue(OUT[answers[0]][inIndex[q]], OutputMbufs[0], uint(count))
							currentState.V.Packets += uint64(count)
						} else if cur.followingNumber == 1 {
							// We have simple handle. Mask will remain the same, current function will be changed
							def[st].f = cur.next[0]
							st++
						} else {
							step := 0
							currentMask = def[st].mask
							for i := uint8(0); i < cur.followingNumber; i++ {
								cont := asm.GenerateMask(&answers, &(vEach[i]), &currentMask, &def[st+step].mask)
								if !cont {
									def[st+step].f = cur.next[i]
									step++
								}
							}
							st += step
						}
						st--
					}
				}
			}
		}
	}
}

func recvRSS(parameters interface{}, inIndex []int32, flag *int32, coreID int) {
	srp := parameters.(*receiveParameters)
	low.ReceiveRSS(uint16(srp.port.PortId), inIndex, srp.out, flag, coreID)
}

func recvKNI(parameters interface{}, inIndex []int32, flag *int32, coreID int) {
	srp := parameters.(*receiveParameters)
	low.ReceiveKNI(uint16(srp.port.PortId), srp.out[0], flag, coreID)
}

func pGenerate(parameters interface{}, inIndex []int32, stopper [2]chan int, report chan reportPair, context []UserContext) {
	// Function is unclonable, report is always nil
	gp := parameters.(*generateParameters)
	OUT := gp.out
	generateFunction := gp.generateFunction
	stopper[1] <- 2 // Answer that function is ready
	for {
		select {
		case <-stopper[0]:
			// It is time to close this clone
			if context[0] != nil {
				context[0].Delete()
			}
			stopper[1] <- 1
			return
		default:
			tempPacket, err := packet.NewPacket()
			if err != nil {
				common.LogFatal(common.Debug, err)
			}
			generateFunction(tempPacket, context[0])
			safeEnqueueOne(OUT[0], tempPacket.ToUintptr())
		}
	}
}

func pFastGenerate(parameters interface{}, inIndex []int32, stopper [2]chan int, report chan reportPair, context []UserContext) {
	gp := parameters.(*generateParameters)
	OUT := gp.out
	generateFunction := gp.generateFunction
	vectorGenerateFunction := gp.vectorGenerateFunction
	mempool := gp.mempool
	vector := (vectorGenerateFunction != nil)

	bufs := make([]uintptr, burstSize)
	var tempPacket *packet.Packet
	tempPackets := make([]*packet.Packet, burstSize)
	var currentState reportPair
	var pause int
	tick := time.NewTicker(time.Duration(schedTime) * time.Millisecond)
	stopper[1] <- 2 // Answer that function is ready
	for {
		select {
		case pause = <-stopper[0]:
			tick.Stop()
			if pause == -1 {
				// It is time to close this clone
				if context[0] != nil {
					context[0].Delete()
				}
				stopper[1] <- 1
				return
			} else {
				// For any events with this function we should restart timer
				// We don't do it regularly without any events due to performance
				tick = time.NewTicker(time.Duration(schedTime) * time.Millisecond)
				currentState = reportPair{}
			}
		case <-tick.C:
			report <- currentState
			currentState = reportPair{}
		default:
			err := low.AllocateMbufs(bufs, mempool, burstSize)
			if err != nil {
				low.ReportMempoolsState()
				common.LogFatal(common.Debug, err)
			}
			if vector == false {
				for i := range bufs {
					// TODO Maybe we need to prefetcht here?
					tempPacket = packet.ExtractPacket(bufs[i])
					generateFunction(tempPacket, context[0])
					if reportMbits {
						currentState.V.Bytes += uint64(tempPacket.GetPacketLen())
					}
				}
			} else {
				packet.ExtractPackets(tempPackets, bufs, burstSize)
				vectorGenerateFunction(tempPackets, context[0])
			}
			safeEnqueue(OUT[0], bufs, burstSize)
			currentState.V.Packets += uint64(burstSize)
			// GO parks goroutines while Sleep. So Sleep lasts more time than our precision
			// we just want to slow goroutine down without parking, so loop is OK for this.
			// time.Now lasts approximately 70ns and this satisfies us
			if pause != 0 {
				a := time.Now()
				for time.Since(a) < time.Duration(pause*int(burstSize))*time.Nanosecond {
				}
			}
		}
	}
}

// TODO reassembled packets are not supported
func pcopy(parameters interface{}, inIndex []int32, stopper [2]chan int, report chan reportPair, context []UserContext) {
	cp := parameters.(*copyParameters)
	IN := cp.in
	OUT := cp.out
	OUTCopy := cp.outCopy
	mempool := cp.mempool

	bufs1 := make([]uintptr, burstSize)
	bufs2 := make([]uintptr, burstSize)
	var tempPacket1 *packet.Packet
	var tempPacket2 *packet.Packet
	var currentState reportPair
	var pause int
	tick := time.NewTicker(time.Duration(schedTime) * time.Millisecond)
	stopper[1] <- 2 // Answer that function is ready

	for {
		select {
		case pause = <-stopper[0]:
			tick.Stop()
			if pause == -1 {
				// It is time to remove this clone
				stopper[1] <- 1
				return
			} else {
				// For any events with this function we should restart timer
				// We don't do it regularly without any events due to performance
				tick = time.NewTicker(time.Duration(schedTime) * time.Millisecond)
				currentState = reportPair{}
			}
		case <-tick.C:
			report <- currentState
			currentState = reportPair{}
		default:
			for q := int32(1); q < inIndex[0]+1; q++ {
				n := IN[inIndex[q]].DequeueBurst(bufs1, burstSize)
				if n != 0 {
					if err := low.AllocateMbufs(bufs2, mempool, n); err != nil {
						common.LogFatal(common.Debug, err)
					}
					for i := uint(0); i < n; i++ {
						// TODO Maybe we need to prefetcht here?
						tempPacket1 = packet.ExtractPacket(bufs1[i])
						tempPacket2 = packet.ExtractPacket(bufs2[i])
						packet.GeneratePacketFromByte(tempPacket2, tempPacket1.GetRawPacketBytes())
						if reportMbits {
							currentState.V.Bytes += uint64(tempPacket1.GetPacketLen())
						}
					}
					safeEnqueue(OUT[inIndex[q]], bufs1, uint(n))
					safeEnqueue(OUTCopy[inIndex[q]], bufs2, uint(n))
					currentState.V.Packets += uint64(n)
				}
				// GO parks goroutines while Sleep. So Sleep lasts more time than our precision
				// we just want to slow goroutine down without parking, so loop is OK for this.
				// time.Now lasts approximately 70ns and this satisfies us
				if pause != 0 {
					currentState.ZeroAttempts[q-1]++
					// pause should be non 0 only if function works with ONE inIndex
					a := time.Now()
					for time.Since(a) < time.Duration(pause*int(burstSize))*time.Nanosecond {
					}
				}
			}
		}
	}
}

func send(parameters interface{}, inIndex []int32, flag *int32, coreID int) {
	srp := parameters.(*sendParameters)
	low.Send(srp.port, srp.queue, srp.in, inIndex[0], flag, coreID)
}

func merge(from low.Rings, to low.Rings) {
	// We should change out rings in all flow functions which we added before
	// and change them to one "after merge" ring.
	// We don't proceed stop and send functions here because they don't have
	// out rings. Also we don't proceed merge function because they are added
	// strictly one after another. The next merge will change previous "after merge"
	// ring automatically.
	for i := range schedState.ff {
		switch parameters := schedState.ff[i].Parameters.(type) {
		case *receiveParameters:
			if parameters.out[0] == from[0] {
				parameters.out = to
			}
		case *generateParameters:
			if parameters.out[0] == from[0] {
				parameters.out = to
			}
		case *readParameters:
			if parameters.out[0] == from[0] {
				parameters.out = to
			}
		case *copyParameters:
			if parameters.out[0] == from[0] {
				parameters.out = to
			}
			if parameters.outCopy[0] == from[0] {
				parameters.outCopy = to
			}
		}
	}
}

func separate(packet *packet.Packet, sc *Func, ctx UserContext) uint {
	return uint(low.BoolToInt(sc.sSeparateFunction(packet, ctx)))
}

func vSeparate(packets []*packet.Packet, mask *[burstSize]bool, answers *[burstSize]uint8, ve *Func, ctx UserContext) {
	ve.vSeparateFunction(packets, mask, low.IntArrayToBool(answers), ctx)
}

// partition doesn't need packets - just mbufs. However it will probably be
// among other functions. So this overhead is not much.
func partition(packet *packet.Packet, sc *Func, ctx UserContext) uint {
	context := ctx.(*partitionCtx)
	context.currentPacketNumber++
	if context.currentPacketNumber == context.currentCompare {
		context.currentAnswer = context.currentAnswer ^ 1
		context.currentCompare = context.N + context.M - context.currentCompare
		context.currentPacketNumber = 0
	}
	return uint(context.currentAnswer)
}

func vPartition(packets []*packet.Packet, mask *[burstSize]bool, answers *[burstSize]uint8, ve *Func, ctx UserContext) {
	context := ctx.(*partitionCtx)
	for i := 0; i < burstSize; i++ {
		if (*mask)[i] {
			context.currentPacketNumber++
			if context.currentPacketNumber == context.currentCompare {
				context.currentAnswer = context.currentAnswer ^ 1
				context.currentCompare = context.N + context.M - context.currentCompare
				context.currentPacketNumber = 0
			}
			answers[i] = context.currentAnswer
		}
	}
}

func split(packet *packet.Packet, sc *Func, ctx UserContext) uint {
	return sc.sSplitFunction(packet, ctx)
}

func vSplit(packets []*packet.Packet, mask *[burstSize]bool, answers *[burstSize]uint8, ve *Func, ctx UserContext) {
	ve.vSplitFunction(packets, mask, answers, ctx)
}

func handle(packet *packet.Packet, sc *Func, ctx UserContext) uint {
	sc.sHandleFunction(packet, ctx)
	return 0
}

func vHandle(packets []*packet.Packet, mask *[burstSize]bool, answers *[burstSize]uint8, ve *Func, ctx UserContext) {
	ve.vHandleFunction(packets, mask, ctx)
}

func constructSlice(packet *packet.Packet, sc *Func, ctx UserContext) uint {
	return sc.bufIndex
}

func vConstructSlice(packets []*packet.Packet, mask *[burstSize]bool, answers *[burstSize]uint8, ve *Func, ctx UserContext) {
	answers[0] = uint8(ve.bufIndex)
}

func write(parameters interface{}, inIndex []int32, stopper [2]chan int) {
	wp := parameters.(*writeParameters)
	IN := wp.in
	filename := wp.filename

	bufIn := make([]uintptr, 1)
	var tempPacket *packet.Packet

	f, err := os.Create(filename)
	if err != nil {
		common.LogFatal(common.Debug, err)
	}
	defer f.Close()

	err = packet.WritePcapGlobalHdr(f)
	if err != nil {
		common.LogFatal(common.Debug, err)
	}
	for {
		select {
		case <-stopper[0]:
			// It is time to close this clone
			stopper[1] <- 1
			return
		default:
			for q := int32(0); q < inIndex[0]; q++ {
				n := IN[q].DequeueBurst(bufIn, 1)
				if n == 0 {
					continue
				}
				tempPacket = packet.ExtractPacket(bufIn[0])
				err := tempPacket.WritePcapOnePacket(f)
				if err != nil {
					common.LogFatal(common.Debug, err)
				}
				low.DirectStop(1, bufIn)
			}
		}
	}
}

func read(parameters interface{}, inIndex []int32, stopper [2]chan int) {
	rp := parameters.(*readParameters)
	OUT := rp.out
	filename := rp.filename
	repcount := rp.repcount

	f, err := os.Open(filename)
	if err != nil {
		common.LogFatal(common.Debug, err)
	}
	defer f.Close()

	// Read pcap global header once
	var glHdr packet.PcapGlobHdr
	if err := packet.ReadPcapGlobalHdr(f, &glHdr); err != nil {
		common.LogFatal(common.Debug, err)
	}

	count := int32(0)

	for {
		select {
		case <-stopper[0]:
			// It is time to close this clone
			stopper[1] <- 1
			return
		default:
			if count >= repcount {
				break
			}
			tempPacket, err := packet.NewPacket()
			if err != nil {
				common.LogFatal(common.Debug, err)
			}
			isEOF, err := tempPacket.ReadPcapOnePacket(f)
			if err != nil {
				common.LogFatal(common.Debug, err)
			}
			if isEOF {
				if atomic.AddInt32(&count, 1) == repcount {
					break
				}
				if _, err := f.Seek(packet.PcapGlobHdrSize, 0); err != nil {
					common.LogFatal(common.Debug, err)
				}
				if _, err := tempPacket.ReadPcapOnePacket(f); err != nil {
					common.LogFatal(common.Debug, err)
				}
			}
			// TODO we need packet reassembly here. However we don't
			// use mbuf packet_type here, so it is impossible.
			safeEnqueueOne(OUT[0], tempPacket.ToUintptr())
		}
	}
}

// This function tries to write elements to input ring. However
// if this ring can't get these elements they will be placed
// inside stop ring which is emptied in separate thread.
func safeEnqueue(place *low.Ring, data []uintptr, number uint) {
	done := place.EnqueueBurst(data, number)
	if done < number {
		schedState.Dropped += number - uint(done)
		done2 := schedState.StopRing[0].EnqueueBurst(data[done:number], number-uint(done))
		// If stop ring is crowded a function will call C stop directly without
		// moving forward. It prevents constant crowd stop and increases
		// performance on "long accelerating" topologies in 1.5x times.
		if done2 < number-uint(done) {
			common.LogWarning(common.Verbose, "Normal fast stop is crowded. Use slow C stop instead.")
			low.DirectStop(int(number-uint(done)-uint(done2)), data[done+done2:number])
		}
	}
	// TODO we need to investigate whether we need to return actual number of enqueued packets.
	// We can use this number if controlling speed, however it is not clear what is better:
	// to use actual number or to use simply number of packets processed by a function like now.
}

// This function makes []uintptr and is inefficient. Only for non-performance critical tasks
func safeEnqueueOne(place *low.Ring, data uintptr) {
	slice := make([]uintptr, 1, 1)
	slice[0] = data
	safeEnqueue(place, slice, 1)
}

func checkFlow(f *Flow) error {
	if f == nil {
		return common.WrapWithNFError(nil, "One of the flows is nil!", common.UseNilFlowErr)
	}
	if f.current == nil && f.previous == nil {
		return common.WrapWithNFError(nil, "One of the flows is used after it was closed!", common.UseClosedFlowErr)
	}
	return nil
}

// CreateKniDevice creates KNI device for using in receive or send functions.
// Gets unique port, and unique name of future KNI device.
func CreateKniDevice(portId uint16, name string) (*Kni, error) {
	if portId >= uint16(len(createdPorts)) {
		return nil, common.WrapWithNFError(nil, "Requested KNI port exceeds number of ports which can be used by DPDK (bind to DPDK).", common.ReqTooManyPorts)
	}
	if createdPorts[portId].willKNI {
		return nil, common.WrapWithNFError(nil, "Requested KNI port already has KNI. Two KNIs for one port are prohibited.", common.MultipleKNIPort)
	}
	if core, coreIndex, err := schedState.getCore(); err != nil {
		return nil, err
	} else {
		if err := low.CreateKni(portId, uint(core), name); err != nil {
			return nil, err
		}
		kni := new(Kni)
		// Port will be identifier of this KNI
		// KNI structure itself is stored inside low.c
		kni.portId = portId
		createdPorts[portId].willKNI = true
		createdPorts[portId].KNICoreIndex = coreIndex
		return kni, nil
	}
}

func FillSliceFromMask(input []uintptr, mask *[burstSize]bool, output []uintptr) uint8 {
	count := 0
	for i := 0; i < burstSize; i++ {
		if (*mask)[i] != false {
			output[count] = input[i]
			count++
		}
	}
	return uint8(count)
}

// AddTimer adds a timer which may call handler function every d milliseconds
// It is required to add at least one variant of this timer for working
// TODO d should be approximate as schedTime because handler will be call from scheduler
// Return created timer
func AddTimer(d time.Duration, handler func(UserContext)) *Timer {
	t := new(Timer)
	t.t = time.NewTicker(d)
	t.handler = handler
	t.contexts = make([]UserContext, 0, 0)
	t.checks = make([]*bool, 0, 0)
	schedState.Timers = append(schedState.Timers, t)
	return t
}

// AddVariant adds a variant for an existing timer. Variant is a context parameter
// which will be passed to handler callback from AddTimer function
// Function return a pointer to variable which should be set to "true"
// everytime to prevent timer from ping.
// Timer variant automatically drops after timer invocation
func (timer *Timer) AddVariant(context UserContext) *bool {
	check := false
	timer.contexts = append(timer.contexts, context)
	timer.checks = append(timer.checks, &check)
	return &check
}

// Stop removes timer with all its variants
func (timer *Timer) Stop() {
	timer.t.Stop()
	for i, t := range schedState.Timers {
		if t == timer {
			schedState.Timers = append(schedState.Timers[:i], schedState.Timers[i+1:]...)
			return
		}
	}
}

// CheckFatal is a default error handling function, which prints error message and
// makes os.Exit in case of non nil error. Any other error handler can be used instead.
func CheckFatal(err error) {
	if err != nil {
		if nfErr := common.GetNFError(err); nfErr != nil {
			common.LogFatalf(common.No, "failed with message and code: %+v\n", nfErr)
		}
		common.LogFatalf(common.No, "failed with message: %s\n", err.Error())
	}
}
