package wsnet

import "github.com/pion/webrtc/v3"

// Message is used for brokering a dialer and listener.
//
// Dialers initiate an exchange by providing an Offer,
// along with a list of ICE servers for the listener to
// peer with.
//
// The listener should respond with an offer, then both
// sides can begin exchanging candidates.
type protoMessage struct {
	// Dialer -> Listener
	Offer   *webrtc.SessionDescription `json:"offer"`
	Servers []webrtc.ICEServer         `json:"servers"`

	// Listener -> Dialer
	Error  string                     `json:"error"`
	Answer *webrtc.SessionDescription `json:"answer"`

	// Bidirectional
	Candidate string `json:"candidate"`
}