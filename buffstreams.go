package buffstreams

import ()

import (
	"encoding/binary"
	"errors"
	"log"
	"net"
	"sync"
)

type BuffManager struct {
	dialedConnections map[string]net.Conn
	listeningSockets  map[string]net.Listener
	// TODO find a way to sanely provide this to a Dialer or a Receiver on a per-connection basis
	MaxMessageSizeBitLength int
	EnableLogging           bool
	// TODO I could control access to the maps better if I centralized how they got accessed - less locking code littered around
	sync.RWMutex
}

type BuffManagerConfig struct {
	MaxMessageSize int
	EnableLogging  bool
}

func New(cfg BuffManagerConfig) *BuffManager {
	bm := &BuffManager{
		dialedConnections: make(map[string]net.Conn),
		listeningSockets:  make(map[string]net.Listener),
		EnableLogging:     cfg.EnableLogging,
	}
	maxMessageSize := 4096
	// 0 is the default, and the message must be atleast 1 byte large
	if cfg.MaxMessageSize != 0 {
		maxMessageSize = cfg.MaxMessageSize
	}
	bm.MaxMessageSizeBitLength = MessageSizeToBitLength(maxMessageSize)
	return bm
}

type ListenCallback func([]byte) error

func formatAddress(address string, port string) string {
	return address + ":" + port
}

func (bm *BuffManager) StartListening(port string, cb ListenCallback) error {
	address := formatAddress("", port)
	receiveSocket, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	bm.startListening(address, receiveSocket, cb)
	return nil
}

func (bm *BuffManager) startListening(address string, socket net.Listener, cb ListenCallback) {
	bm.Lock()
	bm.listeningSockets[address] = socket
	bm.Unlock()

	go func(address string, maxMessageSizeBitLength int, enableLogging bool, listener net.Listener) {
		for {
			// Wait for someone to connect
			conn, err := listener.Accept()
			if err != nil {
				if enableLogging == true {
					log.Print("Error attempting to accept connection")
					log.Print(err)
				}
			} else {
				// Hand this off and immediately listen for more
				go handleListenedConn(address, conn, bm.MaxMessageSizeBitLength, enableLogging, cb)
			}
		}
	}(address, bm.MaxMessageSizeBitLength, bm.EnableLogging, socket)
}

func handleListenedConn(address string, conn net.Conn, maxMessageSize int, enableLogging bool, cb ListenCallback) {
	for {
		// Handle getting the data header
		headerByteSize := maxMessageSize
		headerBuffer := make([]byte, headerByteSize)
		// First, read the number of bytes required to determine the message length
		_, err := readFromConnection(conn, headerBuffer)
		if err != nil && err.Error() == "EOF" {
			// Log the error we got from the call to read
			if enableLogging == true {
				log.Printf("Address %s: Client closed connection", address)
				log.Print(err)
			}
			conn.Close()
			return
		}

		// Now turn that buffer of bytes into an integer - represnts size of message body
		msgLength, bytesParsed := binary.Uvarint(headerBuffer)
		// Not sure what the correct way to handle these errors are. For now, bomb out
		if bytesParsed == 0 {
			// "Buffer too small"
			if enableLogging == true {
				log.Printf("Address %s: 0 Bytes parsed from header", address)
				log.Print(err)
			}
			conn.Close()
			return
		} else if bytesParsed < 0 {
			// "Buffer overflow"
			if enableLogging == true {
				log.Printf("Address %s: Buffer Less than zero bytes parsed from header", address)
				log.Print(err)
			}
			conn.Close()
			return
		}
		dataBuffer := make([]byte, msgLength)
		bytesLen, err := readFromConnection(conn, dataBuffer)
		if err != nil && err.Error() == "EOF" {
			// log the error from the call to read
			if enableLogging == true {
				log.Printf("Address %s: Failure to read from connection", address)
				log.Print(err)
			}
			conn.Close()
			return
		}

		// If we read bytes, there wasn't an error, or if there was it was only EOF
		// And readbytes + EOF is normal, just as readbytes + no err, next read 0 bytes EOF
		// So... we take action on the actual message data
		if bytesLen > 0 && (err == nil || (err != nil && err.Error() == "EOF")) {
			// I ultimately have some design choices here
			// Currently, I am invoking a delegate thats been passed down the stack
			// I could...
			// Just push it onto a queue (not a slow ass channel, but a queue)
			// which has a reference passed down to it, and the main process
			// spawns a goroutine to reap off the queue and handle those in parallel

			// Callback, atm
			err = cb(dataBuffer)
			if err != nil && enableLogging == true {
				log.Printf("Error in Callback")
				log.Print(err)
			}
		}
	}
}

func readFromConnection(reader net.Conn, buffer []byte) (int, error) {
	// This fills the buffer
	bytesLen, err := reader.Read(buffer)
	// Output the content of the bytes to the queue
	if bytesLen == 0 {
		if err != nil && err.Error() == "EOF" {
			// "End of individual transmission"
			// We're just done reading from that conn
			return bytesLen, err
		}
	}

	if err != nil {
		//"Underlying network failure?"
		// Not sure what this error would be, but it could exist and i've seen it handled
		// as a general case in other networking code. Following in the footsteps of (greatness|madness)
	}
	// Read some bytes, return the length
	return bytesLen, nil
}

// If you want to dial out but not immediately write, use this method
func (bm *BuffManager) DialOut(ip string, port string) error {
	address := formatAddress(ip, port)
	bm.RLock()
	if _, ok := bm.dialedConnections[address]; ok == true {
		bm.RUnlock()
		// Need to clean it out on any error...
		return errors.New("You have a connection to this ip and port open already")
	}
	bm.RUnlock()
	tcpAddr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return err
	}
	conn, err := net.DialTCP("tcp", nil, tcpAddr)
	if err != nil {
		return err
	} else {
		// Store the connection, it's valid
		bm.Lock()
		bm.dialedConnections[address] = conn
		bm.Unlock()
	}
	return nil
}

func (bm *BuffManager) CloseDialer(ip string, port string) error {
	address := formatAddress(ip, port)
	bm.Lock()
	if _, ok := bm.dialedConnections[address]; ok != true {
		err := bm.dialedConnections[address].Close()
		delete(bm.dialedConnections, address)
		bm.Unlock()
		return err
	}
	bm.Unlock()
	return nil
}

// Write data and dial out if the conn isn't open
func (bm *BuffManager) WriteTo(ip string, port string, data []byte, persist bool) (int, error) {
	address := formatAddress(ip, port)
	// Get the connection if it's cached, or open a new one
	bm.RLock()
	if _, ok := bm.dialedConnections[address]; ok != true {
		bm.RUnlock()
		err := bm.DialOut(ip, port)
		if err != nil {
			// Error dialing out, cannot write
			// bail
			return 0, err
		}
	} else {
		bm.RUnlock()
	}
	// Calculate how big the message is, using a consistent header size.
	toWriteLen := UInt16ToByteArray(uint16(len(data)), bm.MaxMessageSizeBitLength)
	// Append the size to the message, so now it has a header
	toWrite := append(toWriteLen, data...)
	bm.Lock()
	written, err := bm.dialedConnections[address].Write(toWrite)
	bm.Unlock()
	if err != nil || persist == true {
		err := bm.CloseDialer(ip, port)
		if err != nil {
			// TODO ponder the following:
			// Error closing the dialer, should we still return 0 written?
			// What if some bytes written, then failure, then also the close throws an error
			// []error is a better return type, but not sure if thats a thing you're supposed to do...
			// Possibilities for error not as complicated as i'm thinking?
			if bm.EnableLogging == true {
				// The error will get returned up the stack, no need to log it here?
				log.Print("There was an error writing the message, and a subsequent error cleaning up the connection")
			}
			return 0, err
		}
	}
	// Return the bytes written, any error
	return written, err
}
