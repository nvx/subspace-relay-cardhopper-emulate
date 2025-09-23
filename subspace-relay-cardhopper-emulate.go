package main

import (
	"context"
	"errors"
	"flag"
	"github.com/nvx/go-rfid"
	"github.com/nvx/go-rfid/pm3/cardhopper"
	"github.com/nvx/go-rfid/type4"
	subspacerelay "github.com/nvx/go-subspace-relay"
	srlog "github.com/nvx/go-subspace-relay-logger"
	subspacerelaypb "github.com/nvx/subspace-relay"
	"go.bug.st/serial"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// This can be set at build time using the following go build command:
// go build -ldflags="-X 'main.defaultBrokerURL=mqtts://user:pass@example.com:1234'"
var defaultBrokerURL string

func main() {
	ctx := context.Background()
	var (
		portName   = flag.String("port", os.Getenv("PORT"), "cardhopper serial port")
		brokerFlag = flag.String("broker-url", "", "MQTT Broker URL")
	)
	flag.Parse()

	srlog.InitLogger("subspace-relay-cardhopper-emulate")

	brokerURL := subspacerelay.NotZero(*brokerFlag, os.Getenv("BROKER_URL"), defaultBrokerURL)
	if brokerURL == "" {
		slog.ErrorContext(ctx, "No broker URI specified, either specify as a flag or set the BROKER_URI environment variable")
		flag.Usage()
		os.Exit(2)
	}

	if *portName == "" {
		slog.ErrorContext(ctx, "No port specified, either specify as a flag or set the PORT environment variable")
		flag.Usage()
		os.Exit(2)
	}

	port, err := serial.Open(*portName, &serial.Mode{
		BaudRate: 115200,
	})
	if err != nil {
		slog.ErrorContext(ctx, "Error opening serial port", rfid.ErrorAttrs(err))
		os.Exit(1)
	}
	defer port.Close()

	err = port.SetReadTimeout(100 * time.Millisecond)
	if err != nil {
		slog.ErrorContext(ctx, "Error setting read timeout", rfid.ErrorAttrs(err))
		os.Exit(1)
	}

	h := &handler{}
	h.type4 = &type4.Emulator{Handler: h}
	h.cardhopper = cardhopper.New(port, h.type4)

	h.relay, err = subspacerelay.New(ctx, brokerURL, "")
	if err != nil {
		slog.ErrorContext(ctx, "Error connecting to server", rfid.ErrorAttrs(err))
		os.Exit(1)
	}
	defer h.relay.Close()

	relayInfo := &subspacerelaypb.RelayInfo{
		SupportedPayloadTypes: []subspacerelaypb.PayloadType{subspacerelaypb.PayloadType_PAYLOAD_TYPE_PCSC_CARD},
		ConnectionType:        subspacerelaypb.ConnectionType_CONNECTION_TYPE_NFC,
		SupportsShortcut:      true,
	}

	h.cardRelay = subspacerelay.NewCardRelay[*cardhopper.CardHopper](h.relay, relayInfo, h)

	h.relay.RegisterHandler(h.cardRelay)

	slog.InfoContext(ctx, "Connected, provide the relay_id to your friendly neighbourhood RFID hacker", slog.String("relay_id", h.relay.RelayID))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		interruptChannel := make(chan os.Signal, 10)
		signal.Notify(interruptChannel, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(interruptChannel)

		<-interruptChannel

		_ = h.relay.SendLog(context.WithoutCancel(ctx), &subspacerelaypb.Log{
			Message: "Initiating shutdown",
		})

		slog.InfoContext(ctx, "Signal received, shutting down")
		cancel()
	}()

	err = h.cardRelay.Run(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		_ = h.relay.SendLog(context.WithoutCancel(ctx), &subspacerelaypb.Log{
			Message: "Fatal error during run: " + err.Error(),
		})

		slog.ErrorContext(ctx, "Error", rfid.ErrorAttrs(err))
		os.Exit(1)
	}

	_ = h.relay.SendLog(context.WithoutCancel(ctx), &subspacerelaypb.Log{
		Message: "Shutdown complete",
	})
}

type handler struct {
	relay     *subspacerelay.SubspaceRelay
	cardRelay *subspacerelay.CardRelay[*cardhopper.CardHopper]

	type4      *type4.Emulator
	cardhopper *cardhopper.CardHopper
}

func (h *handler) Exchange(ctx context.Context, capdu []byte) (_ []byte, err error) {
	return h.cardRelay.Exchange(ctx, capdu)
}

func (h *handler) Reset(context.Context) {}

func (h *handler) Setup(ctx context.Context, msg *subspacerelaypb.Reconnect) (_ *cardhopper.CardHopper, err error) {
	defer rfid.DeferWrap(ctx, &err)

	h.type4.UID = msg.Uid
	h.type4.ATS = msg.Ats

	err = h.cardhopper.Setup(ctx)
	if err != nil {
		return
	}

	return h.cardhopper, nil
}

func (h *handler) Run(ctx context.Context, c *cardhopper.CardHopper) error {
	defer c.Close()
	return c.Emulate(ctx)
}
