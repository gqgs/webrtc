package main

import (
	"encoding/json"
	"log"
	"sync/atomic"
	"time"
	"os"
	"io"

	"github.com/pion/webrtc/v2"
	"github.com/pion/datachannel"
)

func check(err error) {
	if err != nil {
		panic(err)
	}
}

func setRemoteDescription(pc *webrtc.PeerConnection, sdp []byte) {
	var desc webrtc.SessionDescription
	err := json.Unmarshal(sdp, &desc)
	check(err)

	// Apply the desc as the remote description
	err = pc.SetRemoteDescription(desc)
	check(err)
}

type FlowControlledDC struct {
	bufferedAmountLowThreshold uint64
	maxBufferedAmount          uint64
	bufferedAmountLowSignal    chan struct{}
	dc                         *webrtc.DataChannel
	detachedDC                 datachannel.ReadWriteCloser
	totalBytesReceived uint64
}

// NewFlowControlledDC --
func NewFlowControlledDC(dc *webrtc.DataChannel, bufferedAmountLowThreshold uint64, maxBufferedAmount uint64) (*FlowControlledDC, error) {
	dcrwc, err := dc.Detach()
	if err != nil {
		return nil, err
	}
	fcdc := &FlowControlledDC{
		dc:                         dc,
		detachedDC:                 dcrwc,
		bufferedAmountLowThreshold: bufferedAmountLowThreshold,
		maxBufferedAmount:          maxBufferedAmount,
		bufferedAmountLowSignal:    make(chan struct{}),
	}
	dc.SetBufferedAmountLowThreshold(bufferedAmountLowThreshold)
	dc.OnBufferedAmountLow(func() {
		fcdc.bufferedAmountLowSignal <- struct{}{}
	})
	return fcdc, nil
}

func (fcdc *FlowControlledDC) Read(p []byte) (int, error) {
	n := len(p)
	atomic.AddUint64(&fcdc.totalBytesReceived, uint64(n))
	return fcdc.detachedDC.Read(p)
}

func (fcdc *FlowControlledDC) Write(p []byte) (int, error) {
	if fcdc.dc.BufferedAmount() > fcdc.maxBufferedAmount {
		<-fcdc.bufferedAmountLowSignal
	}
	return fcdc.detachedDC.Write(p)
}

func createOfferer() *webrtc.PeerConnection {
	s := webrtc.SettingEngine{}
	s.DetachDataChannels()

	// Create an API object with the engine
	api := webrtc.NewAPI(webrtc.WithSettingEngine(s))

	// Prepare the configuration
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{},
	}

	// Create a new PeerConnection
	pc, err := api.NewPeerConnection(config)
	check(err)

	ordered := false
	maxRetransmits := uint16(0)

	options := &webrtc.DataChannelInit{
		Ordered:        &ordered,
		MaxRetransmits: &maxRetransmits,
	}

	// Create a datachannel with label 'data'
	dc, err := pc.CreateDataChannel("data", options)
	check(err)

	// Register channel opening handling
	dc.OnOpen(func() {
		// log.Printf("OnOpen: %s-%d. Start sending a series of 1024-byte packets as fast as it can\n", dc.Label(), dc.ID())
		flowControlledDC, err := NewFlowControlledDC(dc, 512*1024, 1024*1024)
		check(err)

		f, err := os.Open("./test.mp4")
		if err != nil {
			panic(err)
		}
		info, err := f.Stat()
		if err != nil {
			panic(err)
		}
		log.Println(info.Size())
		io.Copy(flowControlledDC, f)
	})
	return pc
}

func createAnswerer() *webrtc.PeerConnection {
	// Prepare the configuration
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{},
	}

	s := webrtc.SettingEngine{}
	s.DetachDataChannels()

	// Create an API object with the engine
	api := webrtc.NewAPI(webrtc.WithSettingEngine(s))
	// Create a new PeerConnection
	pc, err := api.NewPeerConnection(config)
	check(err)

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		// var totalBytesReceived uint64

		// Register channel opening handling
		dc.OnOpen(func() {
			flowControlledDC, err := NewFlowControlledDC(dc, 512*1024, 1024*1024)
			check(err)

			go func() {
				log.Printf("OnOpen: %s-%d. Start receiving data", dc.Label(), dc.ID())
				since := time.Now()

				// Start printing out the observed throughput
				for range time.NewTicker(1000 * time.Millisecond).C {
					bps := float64(atomic.LoadUint64(&flowControlledDC.totalBytesReceived)*8) / time.Since(since).Seconds()
					log.Printf("Throughput: %.03f Mbps, totalBytesReceived: %d", bps/1024/1024, flowControlledDC.totalBytesReceived)
				}
			}()
			f, err := os.OpenFile("/tmp/foo.txt", os.O_CREATE|os.O_RDWR, 0666)
			if err != nil {
				log.Fatal(err)
			}
			io.Copy(f, flowControlledDC)

		})

		// // Register the OnMessage to handle incoming messages
		// dc.OnMessage(func(dcMsg webrtc.DataChannelMessage) {
		// 	n := len(dcMsg.Data)
		// 	atomic.AddUint64(&totalBytesReceived, uint64(n))
		// })
	})

	return pc
}

func main() {
	offerPC := createOfferer()
	answerPC := createAnswerer()

	// Now, create an offer
	offer, err := offerPC.CreateOffer(nil)
	check(err)
	check(offerPC.SetLocalDescription(offer))
	desc, err := json.Marshal(offer)
	check(err)

	setRemoteDescription(answerPC, desc)

	answer, err := answerPC.CreateAnswer(nil)
	check(err)
	check(answerPC.SetLocalDescription(answer))
	desc2, err := json.Marshal(answer)
	check(err)

	setRemoteDescription(offerPC, desc2)

	// Block forever
	select {}
}
