package ugo

import (
	"errors"
	"log"
	//"sort"
	"time"

	"./congestion"
	"./utils"
)

var (
	// ErrDuplicateOrOutOfOrderAck occurs when a duplicate or an out-of-order ACK is received
	ErrDuplicateOrOutOfOrderAck = errors.New("SentPacketHandler: Duplicate or out-of-order ACK")
	// ErrEntropy occurs when an ACK with incorrect entropy is received
	ErrMapAccess = errors.New("Packet does not exist in PacketHistory")
	// ErrTooManyTrackedSentPackets occurs when the sentPacketHandler has to keep track of too many packets
	ErrTooManyTrackedSentPackets = errors.New("Too many outstanding non-acked and non-retransmitted packets")
	errAckForUnsentPacket        = errors.New("Received ACK for an unsent package")
)

var errDuplicatePacketNumber = errors.New("Packet number already exists in Packet History")

type sentPacketHandler struct {
	lastSentPacketNumber uint32
	lastSentPacketTime   time.Time
	largestInOrderAcked  uint32
	largestAcked         uint32

	largestReceivedPacketWithAck uint32

	packetHistory      map[uint32]*Packet
	stopWaitingManager stopWaitingManager

	retransmissionQueue []*Packet

	bytesInFlight uint32

	rttStats   *congestion.RTTStats
	congestion congestion.SendAlgorithm

	totalSend  uint32
	totalAcked uint32
}

// NewSentPacketHandler creates a new sentPacketHandler
func newSentPacketHandler() *sentPacketHandler {
	rttStats := &congestion.RTTStats{}

	congestion := congestion.NewCubicSender(
		congestion.DefaultClock{},
		rttStats,
		false, /* don't use reno since chromium doesn't (why?) */
		InitialCongestionWindow,
		DefaultMaxCongestionWindow,
	)

	return &sentPacketHandler{
		packetHistory:      make(map[uint32]*Packet),
		stopWaitingManager: stopWaitingManager{},
		rttStats:           rttStats,
		congestion:         congestion,
	}
}

func (h *sentPacketHandler) ackPacket(packetNumber uint32) *Packet {
	packet, ok := h.packetHistory[packetNumber]
	if ok && !packet.Retransmitted {
		/*
		* if the packet is marked as retransmitted,
		* it means this packet is queued for retransmission,
		* but now ack for it comes before resending
		 */
		// TODO
		if h.bytesInFlight < packet.Length {
			log.Println("bytes in flight less than send")
			h.bytesInFlight = 0
			h.totalAcked += packet.Length
		} else {
			h.bytesInFlight -= packet.Length
			h.totalAcked += packet.Length
		}
	}

	if h.largestInOrderAcked == packetNumber-1 {
		h.largestInOrderAcked++

		// update stop waiting
		h.stopWaitingManager.largestLeastUnackedSent = h.largestInOrderAcked + 1
	}

	delete(h.packetHistory, packetNumber)

	return packet
}

func (h *sentPacketHandler) nackPacket(packetNumber uint32) (*Packet, error) {
	packet, ok := h.packetHistory[packetNumber]
	// This means that the packet has already been retransmitted, do nothing.
	// We're probably only receiving another NACK for this packet because the
	// retransmission has not yet arrived at the client.
	if !ok {
		return nil, nil
	}

	packet.MissingReports++

	if packet.MissingReports > 3 && !packet.Retransmitted {
		log.Printf("fast retransimition packet %d, Missing count %d", packet.PacketNumber, packet.MissingReports)
		h.queuePacketForRetransmission(packet) // fast retransmition
		return packet, nil
	}
	return nil, nil
}

func (h *sentPacketHandler) queuePacketForRetransmission(packet *Packet) {
	h.bytesInFlight -= packet.Length
	h.retransmissionQueue = append(h.retransmissionQueue, packet)
	packet.Retransmitted = true

	// the packet will be removed when dequeueing

	// increase the LargestInOrderAcked, if this is the lowest packet that hasn't been acked yet
	if packet.PacketNumber == h.largestInOrderAcked+1 {
		h.largestInOrderAcked++
		for i := h.largestInOrderAcked + 1; i <= h.largestAcked; i++ {
			_, ok := h.packetHistory[uint32(i)]
			if !ok {
				h.largestInOrderAcked = i
			} else {
				break
			}
		}
	}

	log.Printf("retransfer packet %d, flag: %d, length %d", packet.PacketNumber, packet.flag, packet.Length)

	// send stopWaiting only when restransmisson happened
	h.stopWaitingManager.SetBoundary(h.largestInOrderAcked)
}

func (h *sentPacketHandler) SentPacket(packet *Packet) error {
	_, ok := h.packetHistory[packet.PacketNumber]
	if ok {
		return errDuplicatePacketNumber
	}

	now := time.Now()
	h.lastSentPacketTime = now
	packet.SendTime = now
	if packet.Length == 0 {
		return errors.New("SentPacketHandler: packet cannot be empty")
	}

	h.lastSentPacketNumber = packet.PacketNumber
	if packet.flag != 0x80 {
		h.totalSend += packet.Length
		h.bytesInFlight += packet.Length
		h.packetHistory[packet.PacketNumber] = packet

		h.congestion.OnPacketSent(
			time.Now(),
			h.BytesInFlight(),
			packet.PacketNumber,
			packet.Length,
			true, /* TODO: is retransmittable */
		)
	}
	return nil
}

func (h *sentPacketHandler) ReceivedAck(ackFrame *AckFrame, withPacketNumber uint32) error {
	if ackFrame.LargestAcked > h.lastSentPacketNumber {
		return errAckForUnsentPacket
	}

	// duplicate or out-of-order ACK
	if withPacketNumber != 0 {
		if withPacketNumber <= h.largestReceivedPacketWithAck {
			return ErrDuplicateOrOutOfOrderAck
		}
	}

	if withPacketNumber != 0 {
		h.largestReceivedPacketWithAck = withPacketNumber
	}

	// ignore repeated or delayed ACK (ACKs that don't have a higher LargestAcked than the last ACK)
	if ackFrame.LargestAcked <= h.largestInOrderAcked {
		return nil
	}

	// out-of-order ACK
	if ackFrame.LargestAcked <= h.largestAcked {
		return nil
	}

	h.largestAcked = ackFrame.LargestAcked

	packet, ok := h.packetHistory[h.largestAcked]
	if ok {
		// Update the RTT
		timeDelta := time.Now().Sub(packet.SendTime)
		// TODO: Don't always update RTT
		h.rttStats.UpdateRTT(timeDelta, ackFrame.DelayTime, time.Now())

		log.Printf("Estimated RTT: %dms", h.rttStats.SmoothedRTT()/time.Millisecond)

	}

	var ackedPackets congestion.PacketVector
	var lostPackets congestion.PacketVector

	// TODO NACK packets below the LowestAcked

	// ACK lost or peer don't update ack range
	for i := h.largestInOrderAcked; i < ackFrame.LargestInOrder; i++ {
		p, err := h.nackPacket(i)
		if err != nil {
			return err
		}
		if p != nil {
			lostPackets = append(lostPackets, congestion.PacketInfo{Number: p.PacketNumber, Length: p.Length})
		}
	}

	ackRangeIndex := 0
	for i := ackFrame.LargestInOrder; i <= ackFrame.LargestAcked; i++ {
		if ackFrame.HasMissingRanges() {
			ackRange := ackFrame.AckRanges[len(ackFrame.AckRanges)-1-ackRangeIndex]

			if i > ackRange.LastPacketNumber && ackRangeIndex < len(ackFrame.AckRanges)-1 {
				ackRangeIndex++
				ackRange = ackFrame.AckRanges[len(ackFrame.AckRanges)-1-ackRangeIndex]
			}

			if i >= ackRange.FirstPacketNumber { // packet i contained in ACK range
				p := h.ackPacket(i)
				if p != nil {
					ackedPackets = append(ackedPackets, congestion.PacketInfo{Number: p.PacketNumber, Length: p.Length})
				}
			} else {
				p, err := h.nackPacket(i)
				if err != nil {
					return err
				}
				if p != nil {
					lostPackets = append(lostPackets, congestion.PacketInfo{Number: p.PacketNumber, Length: p.Length})
				}
			}
		} else {
			p := h.ackPacket(i)
			if p != nil {
				ackedPackets = append(ackedPackets, congestion.PacketInfo{Number: p.PacketNumber, Length: p.Length})
			}
		}
	}

	log.Printf("largest in order send %d, ack in order %d", h.largestInOrderAcked, ackFrame.LargestInOrder)

	h.congestion.OnCongestionEvent(
		true, /* TODO: rtt updated */
		h.BytesInFlight(),
		ackedPackets,
		lostPackets,
	)

	log.Printf("sent %d, acked %d, history size: %d", h.totalSend, h.totalAcked, len(h.packetHistory))

	return nil
}

// ProbablyHasPacketForRetransmission returns if there is a packet queued for retransmission
// There is one case where it gets the answer wrong:
// if a packet has already been queued for retransmission, but a belated ACK is received for this packet, this function will return true, although the packet will not be returend for retransmission by DequeuePacketForRetransmission()
func (h *sentPacketHandler) ProbablyHasPacketForRetransmission() bool {
	h.maybeQueuePacketsRTO()

	return len(h.retransmissionQueue) > 0
}

func (h *sentPacketHandler) DequeuePacketForRetransmission() (packet *Packet) {
	if !h.ProbablyHasPacketForRetransmission() {
		return nil
	}

	for len(h.retransmissionQueue) > 0 {
		queueLen := len(h.retransmissionQueue)
		// packets are usually NACKed in descending order. So use the slice as a stack
		packet = h.retransmissionQueue[queueLen-1]
		h.retransmissionQueue = h.retransmissionQueue[:queueLen-1]

		// this happens if a belated ACK arrives for this packet
		// no need to retransmit it
		_, ok := h.packetHistory[packet.PacketNumber]
		if !ok {
			continue
		}

		delete(h.packetHistory, packet.PacketNumber)
		return packet
	}

	return nil
}

func (h *sentPacketHandler) BytesInFlight() uint32 {
	return h.bytesInFlight
}

func (h *sentPacketHandler) GetLargestAcked() uint32 {
	return h.largestAcked
}

func (h *sentPacketHandler) GetStopWaitingFrame() uint32 {
	return h.stopWaitingManager.GetStopWaitingFrame(false)
}

func (h *sentPacketHandler) CongestionAllowsSending() bool {
	return h.BytesInFlight() <= h.congestion.GetCongestionWindow()
}

func (h *sentPacketHandler) CheckForError() error {
	length := len(h.retransmissionQueue) + len(h.packetHistory)
	if length > 2000 {
		log.Printf("retransmissionQueue size: %d, history size: %d", len(h.retransmissionQueue), len(h.packetHistory))
		return ErrTooManyTrackedSentPackets
	}
	return nil
}

func (h *sentPacketHandler) maybeQueuePacketsRTO() {
	if time.Now().Before(h.TimeOfFirstRTO()) {
		return
	}

	for p := h.largestInOrderAcked + 1; p <= h.lastSentPacketNumber; p++ {
		packet := h.packetHistory[p]
		if packet != nil && !packet.Retransmitted {
			packetsLost := congestion.PacketVector{congestion.PacketInfo{
				Number: packet.PacketNumber,
				Length: packet.Length,
			}}
			h.congestion.OnCongestionEvent(false, h.BytesInFlight(), nil, packetsLost)
			h.congestion.OnRetransmissionTimeout(true)
			log.Printf("timeout retransmission, packet %d, send time:%s, now: %s", packet.PacketNumber, packet.SendTime.String(), time.Now().String())
			h.queuePacketForRetransmission(packet)
			return
		}
	}
}

func (h *sentPacketHandler) getRTO() time.Duration {
	rto := h.congestion.RetransmissionDelay()
	if rto == 0 {
		rto = DefaultRetransmissionTime
	}
	return utils.MaxDuration(rto, MinRetransmissionTime)
}

func (h *sentPacketHandler) TimeOfFirstRTO() time.Time {
	if h.lastSentPacketTime.IsZero() {
		return time.Time{}
	}
	return h.lastSentPacketTime.Add(h.getRTO())
}
