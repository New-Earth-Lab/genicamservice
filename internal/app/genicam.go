package app

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/New-Earth-Lab/bgapi-go/bgapi2"
	"github.com/lirm/aeron-go/aeron"
	"github.com/lirm/aeron-go/aeron/atomic"
	"github.com/lirm/aeron-go/aeron/flyweight"
	"github.com/lirm/aeron-go/aeron/util"
	"golang.org/x/sync/errgroup"
)

type GenICamCamera struct {
	device       *bgapi2.Device
	stream       *bgapi2.DataStream
	publication  *aeron.Publication
	imageBuffer  *atomic.Buffer
	headerBuffer *atomic.Buffer
	header       ImageHeader
}

const (
	RingBufferNumImages = 4
)

type GenICamConfig struct {
	Width        uint32
	Height       uint32
	OffsetX      uint16
	OffsetY      uint16
	SerialNumber string
}

type BgapiConfig struct {
	System       string
	Camera       string
	SerialNumber string
}

func NewGenICam(config GenICamConfig, publication *aeron.Publication) (*GenICamCamera, error) {
	// Get the configuration for the camera
	// Grabber name
	// Camera name
	// ROI - cropping mode
	// Gain

	bgapiConfig := BgapiConfig{
		System:       "libbgapi2_gige.cti",
		Camera:       "Goldeye G-008 Cool (4068580)",
		SerialNumber: "08-406858000765",
	}

	device, err := getDevice(bgapiConfig)
	if err != nil {
		fmt.Println(err)
		os.Exit(0)
	}
	defer func(e *error) {
		if *e != nil {
			releaseResources(device)
		}
	}(&err)

	// Set image size

	// Get image dimensions for buffer size
	// width, height := sdk.GetCurrentImageDimension()
	node, err := device.GetRemoteNode("Width")
	if err != nil {
		return nil, err
	}
	err = node.SetInt(int64(config.Width))
	if err != nil {
		return nil, err
	}

	node, err = device.GetRemoteNode("Height")
	if err != nil {
		return nil, err
	}
	err = node.SetInt(int64(config.Height))
	if err != nil {
		return nil, err
	}

	node, err = device.GetRemoteNode("OffsetX")
	if err != nil {
		return nil, err
	}
	err = node.SetInt(int64(config.OffsetX))
	if err != nil {
		return nil, err
	}

	node, err = device.GetRemoteNode("OffsetY")
	if err != nil {
		return nil, err
	}
	err = node.SetInt(int64(config.OffsetY))
	if err != nil {
		return nil, err
	}

	cam := GenICamCamera{
		device:       device,
		imageBuffer:  new(atomic.Buffer),
		publication:  publication,
		headerBuffer: atomic.MakeBuffer(make([]byte, 256)), // TODO: this is wasteful
	}

	// Set static header information
	cam.header.Wrap(cam.headerBuffer, 0)
	cam.header.Version.Set(0)
	cam.header.PayloadType.Set(0)
	cam.header.Format.Set(0x01100007) // Mono16
	// cam.header.SizeX.Set(int32(width))
	// cam.header.SizeY.Set(int32(height))
	// cam.header.OffsetX.Set(0)
	// cam.header.OffsetY.Set(0)
	cam.header.PaddingX.Set(0)
	cam.header.PaddingY.Set(0)
	cam.header.MetadataLength.Set(0)

	// Just assuming stream 0
	stream, err := device.GetDataStream(0)
	if err != nil {
		return nil, err
	}

	err = stream.Open()
	if err != nil {
		return nil, err
	}

	const bufferCount = 4

	err = addBuffers(stream, bufferCount)
	if err != nil {
		return nil, err
	}

	err = stream.SetNewBufferEventMode(bgapi2.EventMode_EventHandler)
	if err != nil {
		return nil, err
	}

	err = stream.RegisterNewBufferEventHandler(BufferHandler, cam)
	if err != nil {
		return nil, err
	}

	err = stream.StartAcquisitionContinuous()
	if err != nil {
		return nil, err
	}

	cam.stream = stream

	return &cam, nil
}

func (g *GenICamCamera) StartCamera() error {
	node, err := g.device.GetRemoteNode("AcquisitionStart")
	if err != nil {
		return err
	}

	err = node.Execute()
	if err != nil {
		return err
	}

	return nil
}

func (g *GenICamCamera) StopCamera() error {
	node, err := g.device.GetRemoteNode("AcquisitionAbort")
	if err != nil {
		return err
	}

	err = node.Execute()
	if err != nil {
		return err
	}

	node, err = g.device.GetRemoteNode("AcquisitionStop")
	if err != nil {
		return err
	}

	err = node.Execute()
	if err != nil {
		return err
	}

	return nil
}

func (g *GenICamCamera) Shutdown() error {

	err := g.stream.StopAcquisition()
	if err != nil {
		return err
	}

	isGrabbing := true
	for isGrabbing {
		isGrabbing, err = g.stream.IsGrabbing()
		if err != nil {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}

	err = g.stream.SetNewBufferEventMode(bgapi2.EventMode_Polling)
	if err != nil {
		return err
	}

	// Remove buffers
	err = g.stream.Close()
	if err != nil {
		return err
	}

	return nil
}

func (g *GenICamCamera) Run(ctx context.Context) error {
	wg, ctx := errgroup.WithContext(ctx)

	wg.Go(func() error {
		<-ctx.Done()
		return g.Shutdown()
	})
	return wg.Wait()
}

type ImageHeader struct {
	flyweight.FWBase

	Version           flyweight.Int32Field
	PayloadType       flyweight.Int32Field
	TimestampNs       flyweight.Int64Field
	Format            flyweight.Int32Field
	SizeX             flyweight.Int32Field
	SizeY             flyweight.Int32Field
	OffsetX           flyweight.Int32Field
	OffsetY           flyweight.Int32Field
	PaddingX          flyweight.Int32Field
	PaddingY          flyweight.Int32Field
	MetadataLength    flyweight.Int32Field
	MetadataBuffer    flyweight.RawDataField
	pad0              flyweight.Padding
	ImageBufferLength flyweight.Int32Field
}

func (m *ImageHeader) Wrap(buf *atomic.Buffer, offset int) flyweight.Flyweight {
	pos := offset
	pos += m.Version.Wrap(buf, pos)
	pos += m.PayloadType.Wrap(buf, pos)
	pos += m.TimestampNs.Wrap(buf, pos)
	pos += m.Format.Wrap(buf, pos)
	pos += m.SizeX.Wrap(buf, pos)
	pos += m.SizeY.Wrap(buf, pos)
	pos += m.OffsetX.Wrap(buf, pos)
	pos += m.OffsetY.Wrap(buf, pos)
	pos += m.PaddingX.Wrap(buf, pos)
	pos += m.PaddingY.Wrap(buf, pos)
	pos += m.MetadataLength.Wrap(buf, pos)
	pos += m.MetadataBuffer.Wrap(buf, pos, 0)
	pos = int(util.AlignInt32(int32(pos), 4))
	pos += m.ImageBufferLength.Wrap(buf, pos)
	m.SetSize(pos - offset)
	return m
}

func BufferHandler(dataStream *bgapi2.DataStream, buffer *bgapi2.Buffer, userObject any) {
	cam := userObject.(GenICamCamera)

	if val, _ := buffer.GetIsIncomplete(); val {
		return
	} else {
		buf, _ := buffer.GetMemImageBuffer()

		start := time.Now()

		// Get image dimensions for buffer size
		width, _ := buffer.GetWidth()
		height, _ := buffer.GetHeight()
		xOffset, _ := buffer.GetXOffset()
		yOffset, _ := buffer.GetYOffset()
		length := buf.Capacity()

		// Set
		cam.header.TimestampNs.Set(start.UnixNano())
		cam.header.SizeX.Set(int32(width))
		cam.header.SizeY.Set(int32(height))
		cam.header.OffsetX.Set(int32(xOffset))
		cam.header.OffsetY.Set(int32(yOffset))
		cam.header.ImageBufferLength.Set(length)

		const timeout = 100 * time.Microsecond

	out:
		for time.Since(start) < timeout {
			ret := cam.publication.Offer2(cam.headerBuffer, 0,
				int32(cam.header.Size()), buf, 0,
				buf.Capacity(), nil)
			switch ret {
			// Retry on AdminAction and BackPressured
			case aeron.AdminAction, aeron.BackPressured:
				continue
			// Otherwise return as completed
			default:
				break out
			}
		}
	}
	dataStream.QueueBuffer(buffer)
}

func addBuffers(dataStream *bgapi2.DataStream, bufferCount int) error {

	for indexBuffers := 0; indexBuffers < bufferCount; indexBuffers++ {
		buffer, err := bgapi2.CreateBuffer()
		if err != nil {
			return err
		}

		err = dataStream.AnnounceBuffer(buffer)
		if err != nil {
			return err
		}

		dataStream.QueueBuffer(buffer)
		if err != nil {
			return err
		}
	}
	return nil
}

func releaseResources(device *bgapi2.Device) error {
	err := device.Close()
	if err != nil {
		return nil
	}

	iface, err := device.GetParent()
	if err != nil {
		return nil
	}

	err = iface.Close()
	if err != nil {
		return nil
	}

	system, err := iface.GetParent()
	if err != nil {
		return nil
	}

	err = system.Close()
	if err != nil {
		return nil
	}

	err = system.ReleaseSystem()
	if err != nil {
		return nil
	}

	return nil
}

func getDevice(config BgapiConfig) (*bgapi2.Device, error) {
	systems, err := bgapi2.GetSystems()
	if err != nil {
		return nil, err
	}

	var system *bgapi2.System
	for _, sys := range systems {
		fileName, err := sys.GetFileName()
		if err != nil {
			return nil, err
		}

		if fileName == config.System {
			system = sys
			break
		}
	}

	if system == nil {
		return nil, fmt.Errorf("could not find system")
	}

	err = system.Open()
	if err != nil {
		return nil, err
	}
	defer func(e *error) {
		if *e != nil {
			system.Close()
			system.ReleaseSystem()
		}
	}(&err)

	interfaces, err := system.GetInterfaces()
	if err != nil {
		return nil, err
	}

	for _, i := range interfaces {
		err = i.Open()
		if err != nil {
			return nil, err
		}
		defer func(e *error) {
			if *e != nil {
				i.Close()
			}
		}(&err)

		devices, err := i.GetDevices()
		if err != nil {
			return nil, err
		}

		for _, device := range devices {
			err = device.Open()
			if err != nil {
				return nil, err
			}
			defer func(e *error) {
				if *e != nil {
					device.Close()
				}
			}(&err)

			model, err := device.GetModel()
			if err != nil {
				return nil, err
			}

			serialNumber, err := device.GetSerialNumber()
			if err != nil {
				return nil, err
			}

			if model == config.Camera && serialNumber == config.SerialNumber {
				// Found it
				return device, nil
			}

			err = device.Close()
			if err != nil {
				return nil, err
			}
		}

		err = i.Close()
		if err != nil {
			return nil, err
		}
	}

	// Have to set err so cleanup code is called properly
	err = fmt.Errorf("could not find device")

	return nil, err
}
