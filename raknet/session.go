package raknet

import (
	"encoding/hex"
	"fmt"
	"math"
	"math/rand"
	"net"
	"time"

	"github.com/L7-MCPE/lav7/util"
	"github.com/L7-MCPE/lav7/util/buffer"
)

const windowSize = 2048
const chanBufsize = 256

// Sessions contains each raknet client sessions.
var Sessions map[string]*Session
var timeout = time.Second * 5

// GetSession returns session with given identifier if exists, or creates new one.
func GetSession(address *net.UDPAddr, sendChannel chan Packet, playerAdder func(*net.UDPAddr) func(*buffer.Buffer) error) *Session {
	identifier := address.String()
	if s, ok := Sessions[identifier]; ok {
		return s
	}
	util.Debug("New session:", identifier)
	Sessions[identifier] = new(Session)
	Sessions[identifier].Init(address)
	Sessions[identifier].SendChan = sendChannel
	Sessions[identifier].playerAdder = playerAdder
	go Sessions[identifier].work()
	return Sessions[identifier]
}

// Session contains player specific values for raknet-level communication.
type Session struct {
	Status         byte
	ReceivedChan   chan Packet
	SendChan       chan Packet
	ID             uint64
	Address        *net.UDPAddr
	updateTicker   *time.Ticker
	timeout        *time.Timer
	ackQueue       map[uint32]bool
	nackQueue      map[uint32]bool
	mtuSize        uint16
	recovery       map[uint32]*DataPacket
	packetWindow   map[uint32]bool
	windowBorder   [2]uint32 // Window range: [windowBorder[0], windowBorder[1])
	reliableWindow map[uint32]*EncapsulatedPacket
	reliableBorder [2]uint32 // Window range: [windowBorder[0], windowBorder[1])
	splitTable     map[uint16]map[uint32][]byte
	seqNumber      uint32
	lastSeq        uint32
	lastMsgIndex   uint32
	splitID        uint16
	messageIndex   uint32
	channelIndex   [8]uint32
	playerAdder    func(*net.UDPAddr) func(*buffer.Buffer) error
	playerHandler  func(*buffer.Buffer) error
	needPing       uint64
}

// Init sets initial value for session.
func (s *Session) Init(address *net.UDPAddr) {
	s.Address = address
	s.ReceivedChan = make(chan Packet, chanBufsize)
	s.updateTicker = time.NewTicker(time.Millisecond * 100)
	s.timeout = time.NewTimer(time.Millisecond * 1500)
	s.ackQueue = make(map[uint32]bool)
	s.nackQueue = make(map[uint32]bool)
	s.recovery = make(map[uint32]*DataPacket)
	s.packetWindow = make(map[uint32]bool)
	s.reliableWindow = make(map[uint32]*EncapsulatedPacket)
	s.splitTable = make(map[uint16]map[uint32][]byte)
	s.windowBorder = [2]uint32{0, windowSize}
	s.reliableBorder = [2]uint32{0, windowSize}
	s.lastSeq = 1<<32 - 1
	s.lastMsgIndex = 1<<32 - 1
}

func (s *Session) work() {
	for {
		select {
		case pk, ok := <-s.ReceivedChan:
			if !ok {
				return
			}
			s.handlePacket(pk)
		case <-s.updateTicker.C:
			s.update()
		case <-s.timeout.C:
			if s.Status < 3 || s.needPing != 0 {
				s.Close("timeout")
				break
			}
			s.needPing = (uint64(rand.Uint32())<<33 | uint64(rand.Uint32())<<1) + 1
			util.Debug("No signal: sending ping, PingID", s.needPing)
			s.sendEncapsulatedDirect(&EncapsulatedPacket{Buffer: new(ping).Write(Fields{
				"pingID": s.needPing,
			})})
			s.timeout.Reset(timeout)
		}
		if s.ReceivedChan == nil {
			break
		}
	}
}

func (s *Session) update() {
	if len(s.ackQueue) > 0 {
		acks := make([]uint32, len(s.ackQueue))
		i := 0
		for k := range s.ackQueue {
			acks[i] = k
			i++
		}
		buf := EncodeAck(acks)
		b := buffer.FromBytes([]byte{0xc0})
		b.Append(buf)
		s.send(b)
		s.ackQueue = make(map[uint32]bool)
	}
	if len(s.nackQueue) > 0 {
		nacks := make([]uint32, len(s.nackQueue))
		i := 0
		for k := range s.nackQueue {
			nacks[i] = k
			i++
		}
		buf := EncodeAck(nacks)
		b := buffer.FromBytes([]byte{0xa0})
		b.Append(buf)
		s.send(b)
		s.nackQueue = make(map[uint32]bool)
	}
	for seq, pk := range s.recovery {
		if pk.SendTime.Add(time.Second * 8).Before(time.Now()) {
			s.send(pk.Buffer)
			delete(s.recovery, seq)
		} else {
			break
		}
	}
	for seq := range s.packetWindow {
		if seq < s.windowBorder[0] {
			delete(s.packetWindow, seq)
		} else {
			break
		}
	}
	// TODO: Send datapackets from queue
}

func (s *Session) handlePacket(pk Packet) {
	// TODO: Panic recovery
	head := pk.ReadByte()
	if head != 0xa0 && head != 0xc0 {
		s.timeout.Reset(func() time.Duration {
			if s.Status != 3 {
				return time.Millisecond * 1500
			}
			return timeout
		}())
	}
	if handler := GetHandler(head); handler != nil {
		handler.Handle(handler.Read(pk.Buffer), s)
	}
}

func (s *Session) preEncapsulated(ep *EncapsulatedPacket) {
	if ep.Reliability >= 2 && ep.Reliability != 5 { // MessageIndex exists
		if ep.MessageIndex < s.reliableBorder[0] || ep.MessageIndex >= s.reliableBorder[1] { // Outside of window
			//util.Debug("MessageIndex drop:", ep.MessageIndex, "should be", s.reliableBorder[0], "<= n <", s.reliableBorder[1])
			return
		}
		if ep.MessageIndex-s.lastMsgIndex == 1 {
			s.lastMsgIndex++
			s.reliableBorder[0]++
			s.reliableBorder[1]++
			s.handleEncapsulated(ep)
			if len(s.reliableWindow) > 0 {
				for _, i := range util.GetSortedKeys(s.reliableWindow) {
					if uint32(i)-s.lastMsgIndex != 1 {
						break
					}
					s.lastMsgIndex++
					s.reliableBorder[0]++
					s.reliableBorder[1]++
					s.handleEncapsulated(s.reliableWindow[uint32(i)])
					delete(s.reliableWindow, uint32(i))
				}
			}
		} else {
			s.reliableWindow[ep.MessageIndex] = ep
		}
	} else {
		s.handleEncapsulated(ep)
	}
}

func (s *Session) joinSplits(ep *EncapsulatedPacket) {
	if s.Status < 3 {
		return
	}
	tab, ok := s.splitTable[ep.SplitID]
	if !ok {
		util.Debug("New splitID:", ep.SplitID)
		s.splitTable[ep.SplitID] = make(map[uint32][]byte)
		tab = s.splitTable[ep.SplitID]
	}
	if _, ok := tab[ep.SplitIndex]; !ok {
		util.Debug("SplitID:", ep.SplitID, "SplitIndex:", ep.SplitIndex, "SplitCount:", ep.SplitCount)
		tab[ep.SplitIndex] = ep.Buffer.Done()
	}
	if len(tab) == int(ep.SplitCount) {
		util.Debug("Joining")
		ep := new(EncapsulatedPacket)
		ep.Buffer = new(buffer.Buffer)
		for i := uint32(0); i < ep.SplitCount; i++ {
			ep.Write(tab[i])
			fmt.Print(hex.Dump(tab[i]))
		}
		fmt.Print(hex.Dump(ep.Payload))
		s.handleEncapsulated(ep)
	}
}

func (s *Session) handleEncapsulated(ep *EncapsulatedPacket) {
	if ep.HasSplit {
		if s.Status > 2 {
			s.joinSplits(ep)
		}
		return
	}
	head := ep.ReadByte()
	if s.Status > 2 && head >= 0x80 {
		ep.Buffer.Offset = 0
		s.playerHandler(ep.Buffer)
		return
	}
	if handler := GetDataHandler(head); handler != nil {
		handler.Handle(handler.Read(ep.Buffer), s)
	}
}

func (s *Session) connComplete() {
	if s.Status < 3 {
		return
	}
	s.playerHandler = s.playerAdder(s.Address)
}

// SendEncapsulated processes EncapsulatedPacket informations before sending.
func (s *Session) SendEncapsulated(ep *EncapsulatedPacket) {
	if ep.Reliability >= 2 && ep.Reliability != 5 {
		ep.MessageIndex = s.messageIndex
		s.messageIndex++
	}
	if ep.Reliability <= 4 && ep.Reliability != 2 {
		ep.OrderIndex = s.channelIndex[ep.OrderChannel]
		s.channelIndex[ep.OrderChannel]++
	}
	if ep.TotalLen()+4 > int(s.mtuSize) { // Need split
		s.splitID++
		splitID := s.splitID
		splitIndex := uint32(0)
		for !ep.Require(1) {
			readSize := uint32(s.mtuSize) - 34
			if uint32(ep.Buffer.Len())-ep.Offset < readSize {
				readSize = uint32(ep.Buffer.Len()) - ep.Offset
			}
			buf := ep.Read(readSize)
			sp := new(EncapsulatedPacket)
			sp.SplitID = splitID
			sp.HasSplit = true
			sp.SplitCount = uint32(math.Ceil(float64(ep.Buffer.Len()) / float64(s.mtuSize-34)))
			sp.Reliability = ep.Reliability
			sp.SplitIndex = splitIndex
			sp.Buffer = buffer.FromBytes(buf)
			if splitIndex > 0 {
				sp.MessageIndex = s.messageIndex
				s.messageIndex++
			} else {
				sp.MessageIndex = s.messageIndex
			}
			if sp.Reliability == 3 {
				sp.OrderChannel = ep.OrderChannel
				sp.OrderIndex = ep.OrderIndex
			}
			splitIndex++
			s.sendEncapsulatedDirect(sp)
		}
	} else {
		s.sendEncapsulatedDirect(ep)
	}
}

func (s *Session) sendEncapsulatedDirect(ep *EncapsulatedPacket) {
	dp := new(DataPacket)
	dp.Head = 0x80
	dp.SeqNumber = s.seqNumber
	s.seqNumber++
	dp.Packets = []*EncapsulatedPacket{ep}
	dp.Encode()
	s.send(dp.Buffer)
	dp.SendTime = time.Now()
	s.recovery[dp.SeqNumber] = dp
}

func (s *Session) send(pk *buffer.Buffer) {
	s.SendChan <- Packet{pk, s.Address}
}

// Close stops current session
func (s *Session) Close(reason string) {
	if s.ReceivedChan == nil {
		return
	}
	data := &EncapsulatedPacket{Buffer: buffer.FromBytes([]byte{0x15})}
	s.sendEncapsulatedDirect(data)
	s.updateTicker.Stop()
	s.ReceivedChan = nil
	delete(Sessions, s.Address.String())
	delete(Players, s.Address.String())
	blockList[s.Address.String()] = time.Now().Add(time.Second + time.Millisecond*500)
	util.Debug("Session closed:", reason)
}