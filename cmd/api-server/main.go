package main

import (
	"context"
	"flag"
	"net/http"

	"github.com/go-faster/errors"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/New-Earth-Lab/genicamservice/internal/api"
	"github.com/New-Earth-Lab/genicamservice/internal/app"
	"github.com/New-Earth-Lab/genicamservice/internal/oas"
	"github.com/lirm/aeron-go/aeron"
)

func main() {
	app.Run(func(ctx context.Context, lg *zap.Logger) error {
		var arg struct {
			Addr               string
			MetricsAddr        string
			AeronUri           string
			AeronStreamId      int
			CameraSerialNumber string
			Width              int
			Height             int
			OffsetX            int
			OffsetY            int
		}
		flag.StringVar(&arg.Addr, "addr", "127.0.0.1:8082", "listen address")
		flag.StringVar(&arg.MetricsAddr, "metrics.addr", "127.0.0.1:9092", "metrics listen address")
		flag.StringVar(&arg.AeronUri, "aeron.Uri", "aeron:ipc", "Aeron channel URI")
		flag.IntVar(&arg.AeronStreamId, "aeron.StreamId", 1003, "Aeron stream ID")
		flag.StringVar(&arg.CameraSerialNumber, "serialNumber", "08-406858000765", "Camera Serial Number")
		flag.IntVar(&arg.Width, "width", 206, "Image width")
		flag.IntVar(&arg.Height, "height", 125, "Image height")
		flag.IntVar(&arg.OffsetX, "offsetx", 54, "Image X offset")
		flag.IntVar(&arg.OffsetY, "offsety", 63, "Image Y offset")

		flag.Parse()

		lg.Info("Initializing",
			zap.String("http.addr", arg.Addr),
			zap.String("metrics.addr", arg.MetricsAddr),
			zap.String("aeron.Uri", arg.AeronUri),
			zap.Int("aeron.streamId", arg.AeronStreamId),
			zap.String("serialNumber", arg.CameraSerialNumber),
		)

		metrics, err := app.NewMetrics(lg, app.Config{
			Addr: arg.MetricsAddr,
			Name: "api",
		})
		if err != nil {
			return errors.Wrap(err, "metrics")
		}

		oasServer, err := oas.NewServer(api.Handler{},
			oas.WithTracerProvider(metrics.TracerProvider()),
			oas.WithMeterProvider(metrics.MeterProvider()),
		)
		if err != nil {
			return errors.Wrap(err, "server init")
		}
		httpServer := http.Server{
			Addr:    arg.Addr,
			Handler: oasServer,
		}

		aeronContext := aeron.NewContext()

		a, err := aeron.Connect(aeronContext)
		if err != nil {
			return errors.Wrap(err, "aeron connect")
		}
		defer a.Close()

		publication, err := a.AddPublication(arg.AeronUri, int32(arg.AeronStreamId))
		if err != nil {
			return errors.Wrap(err, "aeron AddPublication")
		}
		defer publication.Close()

		g, ctx := errgroup.WithContext(ctx)
		g.Go(func() error {
			return metrics.Run(ctx)
		})
		g.Go(func() error {
			camConfig := app.GenICamConfig{
				Width:        uint32(arg.Width),
				Height:       uint32(arg.Height),
				OffsetX:      uint16(arg.OffsetX),
				OffsetY:      uint16(arg.OffsetY),
				SerialNumber: arg.CameraSerialNumber,
			}

			cam, err := app.NewGenICam(camConfig, publication)
			if err != nil {
				return errors.Wrap(err, "genicam")
			}
			if err := cam.StartCamera(); err != nil {
				return errors.Wrap(err, "genicam")
			}
			return cam.Run(ctx)
		})
		g.Go(func() error {
			<-ctx.Done()
			if err := httpServer.Shutdown(ctx); err != nil {
				return errors.Wrap(err, "http")
			}
			return nil
		})
		g.Go(func() error {
			defer lg.Info("HTTP server stopped")
			if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				return errors.Wrap(err, "http")
			}
			return nil
		})

		return g.Wait()
	})
}
