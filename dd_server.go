package main

import (
	"fmt"
	"net/http"
	"runtime"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  256,
	WriteBufferSize: 256,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

func handler(newConn chan *websocket.Conn, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Println(err)
		return
	}
	newConn <- conn
}

type messageRequest struct {
	read      bool
	messageID int
	senderID  int
	message   []byte
	response  chan []*gameMessage
}
type playerRequest struct {
	disconnect bool
	playerID   int
	response   chan *playerConnection
}
type connectionRequest struct {
	reconnect bool
	playerId  int
	message   *websocket.Conn
	response  chan *stateConnection
}
type playerConnection struct {
	playerId int
	conn     *websocket.Conn
	active   bool
	inputs   chan *gameMessage
	close    chan bool
}
type stateConnection struct {
	player   playerConnection
	pRequest chan *playerRequest
	mRequest chan *messageRequest
}
type gameMessage struct {
	messageID int
	message   []byte
}
type wsMsg struct {
	mT  int
	p   []byte
	err error
}

var gameActive bool = false

func GameState(mRequest chan *messageRequest, cPlayers chan *connectionRequest, pRequest chan *playerRequest) {
	knownConnections := make(map[int]*playerConnection)
	var maxID int = 0
	var pastMessages []*gameMessage
	broadcastMessage := func(mR *messageRequest) {
		fmt.Println("message broadcasted")
		for kC := range knownConnections {
			pC := knownConnections[kC]
			if pC.playerId != mR.senderID && pC.active {
				go func() {
					select {
					case pC.inputs <- &gameMessage{message: mR.message}:
					default:
						pC.active = false
						pC.close <- true
						delete(knownConnections, pC.playerId)
					}
				}()
			}
		}
	}
	for {
		select {
		case mR := <-mRequest:
			if mR.read {
				mSlice := pastMessages[mR.messageID:]
				fmt.Println("messages to send: ", len(mSlice))
				mR.response <- mSlice
			} else {
				msg := gameMessage{
					messageID: len(pastMessages),
					message:   mR.message,
				}
				msg.message[2] = byte(msg.messageID >> 8)
				msg.message[3] = byte(msg.messageID & 255)
				fmt.Println(len(pastMessages))
				pastMessages = append(pastMessages, &msg)
				broadcastMessage(mR)
			}
		case cR := <-cPlayers:
			if cR.reconnect {
				/*if knownConnections[cR.playerId].active {
				}*/
			} else {
				nC := playerConnection{
					playerId: maxID,
					conn:     cR.message,
					active:   true,
					inputs:   make(chan *gameMessage),
					close:    make(chan bool, 1),
				}
				knownConnections[maxID] = &nC
				maxID++
				fmt.Println(len(knownConnections), " ", maxID)
				sC := stateConnection{
					player:   nC,
					pRequest: pRequest,
					mRequest: mRequest,
				}
				cR.response <- &sC
				broadcastMessage(&messageRequest{read: false, message: []byte{160 | byte(len(knownConnections)>>8), byte(len(knownConnections) & 255)}, senderID: maxID - 1})
			}
		case pR := <-pRequest:
			if pR.disconnect {
				pC := knownConnections[pR.playerID]
				delete(knownConnections, pC.playerId)
				broadcastMessage(&messageRequest{read: false, message: []byte{160 | byte(len(knownConnections)>>8), byte(len(knownConnections) & 255)}, senderID: pC.playerId})
				pC.close <- true
				fmt.Println("clients still connected ", len((knownConnections)))
				if len(knownConnections) <= 0 {
					done <- true
				}
			} else {
				pR.response <- knownConnections[pR.playerID]
			}
		case <-done:
			fmt.Println("game done")
			for _, kC := range knownConnections {
				select {
				case kC.close <- true:
				default:
				}
			}
			return
		}
	}
}
func HandleNewConnection(newConn chan *websocket.Conn, cRequest chan *connectionRequest) {
	for nC := range newConn {
		fmt.Println(nC.RemoteAddr().String())
		_, p, err := nC.ReadMessage()
		if err != nil {
			fmt.Println("error ", err)
			nC.Close()
			continue
		}
		fmt.Println("message ", p)
		if (p[0]>>6)&3 == 0 { //&& (p[0]>>4)&3 == 0
			//00 (message Type) //00 (no game in progress) 01 (Game in lobby/progress) //playerId
			connectResponse := make(chan *stateConnection)
			nR := connectionRequest{
				reconnect: false,
				playerId:  0,
				message:   nC,
				response:  connectResponse,
			}
			cRequest <- &nR
			pC := <-connectResponse
			close(connectResponse)
			go ClientIO(pC)
			rBytes := []byte{(3 << 4) | byte(pC.player.playerId>>8), byte(pC.player.playerId)}
			//fmt.Println("sent: ", rBytes)
			//nC.WriteMessage(websocket.BinaryMessage, rBytes)
			pC.player.inputs <- &gameMessage{message: rBytes}
		} else {
			nC.Close()
		}
	}
}
func WsRead(read chan wsMsg, ws *websocket.Conn, eOut chan error, readEnd chan bool) {
	ws.SetPongHandler(func(string) error { return nil })
	for gameActive {
		mT, p, err := ws.ReadMessage()
		select {
		case <-readEnd:
			return
		default:
		}
		if err != nil {
			fmt.Println("error wsRead", err)
			eOut <- err
			return
		}
		fmt.Println("message ", p)
		if mT == websocket.PingMessage || mT == websocket.PongMessage {
			return
		}
		wsM := wsMsg{
			mT:  mT,
			p:   p,
			err: err,
		}

		read <- wsM
	}
}
func ClientIO(state *stateConnection) {
	fmt.Println("client started")
	errorOut := make(chan error)
	readIn := make(chan wsMsg)
	readEnd := make(chan bool)
	defer close(readIn)
	defer close(errorOut)
	go WsRead(readIn, state.player.conn, errorOut, readEnd)
	for gameActive {
		select {
		case gM := <-state.player.inputs:
			fmt.Println("sent to client ", state.player.playerId, ": ", gM.message)
			state.player.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			err := state.player.conn.WriteMessage(websocket.BinaryMessage, gM.message)
			if err != nil {
				fmt.Println("send error ", err)
				state.player.close <- true
			}
		case mT := <-readIn:
			switch (mT.p[0] >> 6) & 3 {
			case 1:
				pR := messageRequest{message: mT.p, senderID: state.player.playerId}
				state.mRequest <- &pR
			case 2:
				fmt.Println("client missed messages")
				msgResponse := make(chan []*gameMessage)
				mID := (int(mT.p[2]) << 8) | int(mT.p[3])
				fmt.Println("last message", mID)
				state.mRequest <- &messageRequest{read: true, messageID: int(mID), response: msgResponse}
				missedMsg := <-msgResponse
				for mM := range missedMsg {
					m := missedMsg[mM]
					fmt.Println("catch up to client: ", m.messageID, " -> ", m.message)
					state.player.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
					err := state.player.conn.WriteMessage(websocket.BinaryMessage, m.message)
					if err != nil {
						fmt.Println("send error ", err)
						state.player.close <- true
					}
				}
				fmt.Println("all messages sent to client")
			}
		case <-errorOut:
			fmt.Println("client error closed ", state.player.playerId)
			pR := playerRequest{disconnect: true, playerID: state.player.playerId}
			state.pRequest <- &pR
		case <-state.player.close:
			fmt.Println("client closed ", state.player.playerId)
			state.player.conn.Close()
			readEnd <- true
			return
		}
	}
}
func GameMain() {
	newConnection := make(chan *websocket.Conn)
	mRequest := make(chan *messageRequest)
	cRequest := make(chan *connectionRequest)
	pRequest := make(chan *playerRequest)
	defer close(newConnection)
	defer close(mRequest)
	defer close(cRequest)
	defer close(pRequest)
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		handler(newConnection, w, r)
	})
	go HandleNewConnection(newConnection, cRequest)
	go http.ListenAndServe(":10000", nil)
	for {
		fmt.Println("Game Start")
		gameActive = true
		GameState(mRequest, cRequest, pRequest)
		gameActive = false
		fmt.Println("Game End", runtime.NumGoroutine())
	}
}

var done = make(chan bool, 1)

func main() {
	GameMain()

}
