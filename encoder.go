// Copyright 2021 Grabtaxi Holdings Pte Ltd (GRAB), All rights reserved.

// Use of this source code is governed by an MIT-style license that can be found in the LICENSE file

package gosm

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"sync"

	"github.com/MichielMortier/gosm/gosmpb"
	"github.com/golang/protobuf/proto"
)

const (
	blobTypeHeader = "OSMHeader"
	blobTypeData   = "OSMData"
	logTag         = "gosm"

	defaultLimitNumberInOnePrimitiveGroup = 8000
)

// Encoder contains all the needed context when generating pbf file.
type Encoder struct {
	bbox             *gosmpb.HeaderBBox
	requiredFeatures []string
	optionalFeatures []string
	writingProgram   string
	enableZlip       bool

	writer io.WriteCloser

	errs          chan error
	writeBuf      chan *gosmpb.PrimitiveBlock
	nodesBuf      chan members
	waysBuf       chan members
	relationsBuf  chan members
	nodeFlush     chan chan struct{}
	wayFlush      chan chan struct{}
	relationFlush chan chan struct{}

	wg    sync.WaitGroup
	errWg sync.WaitGroup

	logger logger
}

type members interface {
	toPrimitiveBlock() (*gosmpb.PrimitiveBlock, error)
	len() int
	appendMembers(members)
	clear()
}

// NewEncoderRequiredInput contains the required parameters to initialize an encoder
type NewEncoderRequiredInput struct {
	RequiredFeatures []string
	Writer           io.WriteCloser
}

// NewEncoder initializes an OSM pbf encoder.
func NewEncoder(input *NewEncoderRequiredInput, opts ...Option) *Encoder {
	encoder := &Encoder{
		requiredFeatures: input.RequiredFeatures,
		writer:           input.Writer,

		writeBuf:      make(chan *gosmpb.PrimitiveBlock),
		errs:          make(chan error),
		nodesBuf:      make(chan members),
		waysBuf:       make(chan members),
		relationsBuf:  make(chan members),
		nodeFlush:     make(chan chan struct{}),
		wayFlush:      make(chan chan struct{}),
		relationFlush: make(chan chan struct{}),
	}

	encoder.enableZlip = true
	for _, opt := range opts {
		opt(encoder)
	}

	if encoder.logger == nil {
		encoder.logger = log.New(os.Stderr, logTag, log.LstdFlags)
	}

	return encoder
}

func (e *Encoder) processMembers(membersBufChan chan members, flushChan chan chan struct{}, memberType string) {
	var appendedMembers members
	defer e.wg.Done()

	flush := func() {
		if appendedMembers != nil {
			pgs, err := appendedMembers.toPrimitiveBlock()
			if err != nil {
				e.errs <- fmt.Errorf("flush %s: %w", memberType, err)
				return
			}
			e.writeBuf <- pgs
			appendedMembers.clear()
		}
	}

	for {
		select {
		// flush data below defaultLimitNumberInOnePrimitiveGroup
		case done, ok := <-flushChan:
			if !ok {
				return
			}
			flush()
			close(done)
		case c, ok := <-membersBufChan:
			if appendedMembers == nil && ok {
				appendedMembers = c
				continue
			}
			// when channel is closed, need to flush
			if !ok || appendedMembers.len()+c.len() > defaultLimitNumberInOnePrimitiveGroup {
				flush()
			}
			if appendedMembers != nil {
				appendedMembers.appendMembers(c)
			}
			if !ok {
				return
			}
		}
	}
}

// Start will write the header file to the writer and start consuming data channel and write to the writer.
func (e *Encoder) Start() (chan error, error) {
	e.errWg.Add(1)
	go func() {
		for {
			d, ok := <-e.writeBuf
			if !ok {
				// no err data to write, can close err chan now
				e.errWg.Done()
				return
			}
			encodedBlob, err := proto.Marshal(d)
			if err != nil {
				e.errs <- fmt.Errorf("marshal blob data: %w", err)
			}
			if err := e.encodeBlockToBlob(encodedBlob, blobTypeData); err != nil {
				e.errs <- fmt.Errorf("encode data block :%w", err)
			}
		}
	}()
	e.wg.Add(3)
	go e.processMembers(e.nodesBuf, e.nodeFlush, "osm node")
	go e.processMembers(e.waysBuf, e.wayFlush, "osm ways")
	go e.processMembers(e.relationsBuf, e.relationFlush, "osm relations")

	// write file header
	header := &gosmpb.HeaderBlock{
		Bbox:             e.bbox,
		RequiredFeatures: e.requiredFeatures,
		OptionalFeatures: e.optionalFeatures,
	}
	if e.writingProgram == "" {
		header.Writingprogram = nil
	} else {
		header.Writingprogram = &e.writingProgram
	}
	encodedHeader, err := proto.Marshal(header)
	if err != nil {
		return nil, fmt.Errorf("marshal file header: %w", err)
	}

	if err := e.encodeBlockToBlob(encodedHeader, blobTypeHeader); err != nil {
		return nil, fmt.Errorf("encode blob header: %w", err)
	}
	return e.errs, nil
}

// Close will stop consuming the channel and close the writer.
func (e *Encoder) Close() error {
	defer func() {
		if res := recover(); res != nil {
			e.logger.Printf("%s close chan panic, panic:%+v", logTag, res)
		}
	}()

	close(e.nodesBuf)
	close(e.waysBuf)
	close(e.relationsBuf)
	e.wg.Wait()
	close(e.writeBuf)
	e.errWg.Wait()
	close(e.errs)
	return e.writer.Close()
}

// Flush consume the remaining data from the buffer immediately and writes to the pbf file.
func (e *Encoder) Flush(memberType MemberType) {
	defer func() {
		if res := recover(); res != nil {
			e.logger.Printf("%s, the flush have been closed, panic:%+v", logTag, res)
		}
	}()
	// make sure to flush data before append a new element
	switch memberType {
	case NodeType:
		done := make(chan struct{})
		e.nodeFlush <- done
		<-done
	case WayType:
		done := make(chan struct{})
		e.wayFlush <- done
		<-done
	case RelationType:
		done := make(chan struct{})
		e.relationFlush <- done
		<-done
	}
}

// encodeBlockToBlob wraps the encoded data into blob and write blob header length, blob header and blob to writer
// return n bytes written and error.
func (e *Encoder) encodeBlockToBlob(p []byte, blobType string) error {
	blob := &gosmpb.Blob{}
	blob.RawSize = countInt32LenOfBytes(p)
	if e.enableZlip {
		var b bytes.Buffer
		w := zlib.NewWriter(&b)
		if _, err := w.Write(p); err != nil {
			return fmt.Errorf("compress block: %w", err)
		}
		if err := w.Close(); err != nil {
			return fmt.Errorf("close zlib writer: %w", err)
		}
		blob.ZlibData = b.Bytes()
	} else {
		blob.Raw = p
	}
	encodedBlob, err := proto.Marshal(blob)
	if err != nil {
		return fmt.Errorf("marshal blob: %w", err)
	}

	blobHeader := &gosmpb.BlobHeader{
		Type:     stringToPointer(blobType),
		Datasize: countInt32LenOfBytes(encodedBlob),
	}
	encodedBlobHeader, err := proto.Marshal(blobHeader)
	if err != nil {
		return fmt.Errorf("marshal blob header: %w", err)
	}

	blobHeaderSize := uint32(len(encodedBlobHeader))
	headerLengthInNetworkByte := make([]byte, 4) // uint32 takes 4 bytes
	binary.BigEndian.PutUint32(headerLengthInNetworkByte, blobHeaderSize)
	if _, err = e.writer.Write(headerLengthInNetworkByte); err != nil {
		return fmt.Errorf("write header length: %w", err)
	}
	if _, err = e.writer.Write(encodedBlobHeader); err != nil {
		return fmt.Errorf("write blob header: %w", err)
	}
	if _, err = e.writer.Write(encodedBlob); err != nil {
		return fmt.Errorf("write blob: %w", err)
	}
	return nil
}
