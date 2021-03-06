package author

import (
	"io"
	"fmt"
	"github.com/drausin/libri/libri/author/io/enc"
	"github.com/drausin/libri/libri/author/io/pack"
	"github.com/drausin/libri/libri/author/io/page"
	"github.com/drausin/libri/libri/author/io/publish"
	"github.com/drausin/libri/libri/author/io/ship"
	"github.com/drausin/libri/libri/author/keychain"
	"github.com/drausin/libri/libri/common/db"
	"github.com/drausin/libri/libri/common/ecid"
	"github.com/drausin/libri/libri/common/id"
	"github.com/drausin/libri/libri/common/storage"
	"github.com/drausin/libri/libri/librarian/api"
	"github.com/drausin/libri/libri/librarian/client"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"go.uber.org/zap"
	"time"
	"golang.org/x/net/context"
	"github.com/dustin/go-humanize"
	"crypto/ecdsa"
)

const (
	// LoggerEntryKey is the logger key used for the key of an Entry document.
	LoggerEntryKey = "entry_key"

	// LoggerEnvelopeKey is the logger key used for the key of an Envelope document.
	LoggerEnvelopeKey = "envelope_key"

	// LoggerAuthorPub is the logger key used for an author public key.
	LoggerAuthorPub = "author_pub"

	// LoggerReaderPub is the logger key used for a reader public key.
	LoggerReaderPub = "reader_pub"

	// LoggerNPages is the logger key used for the number of pages in a document.
	LoggerNPages = "n_pages"
)

var (
	healthcheckTimeout = 2 * time.Second
)

// Author is the main client of the libri network. It can upload, download, and share documents with
// other author clients.
type Author struct {
	// selfID is ID of this author client
	clientID ecid.ID

	// Config holds the configuration parameters of the server
	config *Config

	// collection of keys for encrypting Envelope documents; these can be used as either the
	// author or reader keys
	authorKeys keychain.GetterSampler

	// collection of reader keys used with sending Envelope documents to oneself; these are
	// never used as the author key
	selfReaderKeys keychain.Getter

	// union of authorKeys and selfReaderKeys
	allKeys keychain.Getter

	// samples a pair of author and selfReader keys for encrypting an entry
	envKeys envelopeKeySampler

	// key-value store DB used for all external storage
	db db.KVDB

	// SL for client data
	clientSL storage.NamespaceSL

	// SLD for locally stored documents
	documentSLD storage.DocumentSLD

	// load balancer for librarian clients
	librarians api.ClientBalancer

	// librarian address -> health check client for all librarians
	librarianHealths map[string]healthpb.HealthClient

	// creates entry documents from raw content
	entryPacker pack.EntryPacker

	entryUnpacker pack.EntryUnpacker

	// publishes documents to libri
	shipper ship.Shipper

	receiver ship.Receiver

	// stores Pages in chan to local storage
	pageSL page.StorerLoader

	// signs requests
	signer client.Signer

	// logger for this instance
	logger *zap.Logger

	// receives graceful stop signal
	stop chan struct{}
}

// NewAuthor creates a new *Author from the Config, decrypting the keychains with the supplied
// auth string.
func NewAuthor(
	config *Config,
	authorKeys keychain.GetterSampler,
	selfReaderKeys keychain.GetterSampler,
	logger *zap.Logger) (*Author, error) {

	rdb, err := db.NewRocksDB(config.DbDir)
	if err != nil {
		logger.Error("unable to init RocksDB", zap.Error(err))
		return nil, err
	}
	clientSL := storage.NewClientSL(rdb)
	documentSL := storage.NewDocumentSLD(rdb)

	// get client ID and immediately save it so subsequent restarts have it
	clientID, err := loadOrCreateClientID(logger, clientSL)
	if err != nil {
		return nil, err
	}

	allKeys := keychain.NewUnion(authorKeys, selfReaderKeys)
	envKeys := &envelopeKeySamplerImpl{
		authorKeys:     authorKeys,
		selfReaderKeys: selfReaderKeys,
	}
	librarians, err := api.NewUniformRandomClientBalancer(config.LibrarianAddrs)
	if err != nil {
		return nil, err
	}
	librarianHealths, err := getLibrarianHealthClients(config.LibrarianAddrs)
	if err != nil {
		return nil, err
	}
	signer := client.NewSigner(clientID.Key())

	publisher := publish.NewPublisher(clientID, signer, config.Publish)
	acquirer := publish.NewAcquirer(clientID, signer, config.Publish)
	slPublisher := publish.NewSingleLoadPublisher(publisher, documentSL)
	ssAcquirer := publish.NewSingleStoreAcquirer(acquirer, documentSL)
	mlPublisher := publish.NewMultiLoadPublisher(slPublisher, config.Publish)
	msAcquirer := publish.NewMultiStoreAcquirer(ssAcquirer, config.Publish)
	shipper := ship.NewShipper(librarians, publisher, mlPublisher)
	receiver := ship.NewReceiver(librarians, allKeys, acquirer, msAcquirer, documentSL)

	mdEncDec := enc.NewMetadataEncrypterDecrypter()
	entryPacker := pack.NewEntryPacker(config.Print, mdEncDec, documentSL)
	entryUnpacker := pack.NewEntryUnpacker(config.Print, mdEncDec, documentSL)

	author := &Author{
		clientID:         clientID,
		config:           config,
		authorKeys:       authorKeys,
		selfReaderKeys:   selfReaderKeys,
		allKeys:          allKeys,
		envKeys:     envKeys,
		db:               rdb,
		clientSL:         clientSL,
		documentSLD:      documentSL,
		librarians:       librarians,
		librarianHealths: librarianHealths,
		entryPacker:      entryPacker,
		entryUnpacker:    entryUnpacker,
		shipper:          shipper,
		receiver:         receiver,
		pageSL:           page.NewStorerLoader(documentSL),
		signer:           signer,
		logger:           logger,
		stop:             make(chan struct{}),
	}

	// for now, this doesn't really do anything
	go func() { <-author.stop }()

	return author, nil
}

// Healthcheck executes and reports healthcheck status for all connected librarians.
func (a *Author) Healthcheck() (bool, map[string]healthpb.HealthCheckResponse_ServingStatus) {
	healthStatus := make(map[string]healthpb.HealthCheckResponse_ServingStatus)
	allHealthy := true
	for addrStr, healthClient := range a.librarianHealths {
		ctx, cancel := context.WithTimeout(context.Background(), healthcheckTimeout)
		rp, err := healthClient.Check(ctx, &healthpb.HealthCheckRequest{})
		cancel()
		if err != nil {
			healthStatus[addrStr] = healthpb.HealthCheckResponse_UNKNOWN
			allHealthy = false
			a.logger.Info("librarian peer is not reachable",
				zap.String("peer_address", addrStr),
			)
			continue
		}

		healthStatus[addrStr] = rp.Status
		if rp.Status == healthpb.HealthCheckResponse_SERVING {
			a.logger.Info("librarian peer is healthy",
				zap.String("peer_address", addrStr),
			)
			continue
		}

		allHealthy = false
		a.logger.Warn("librarian peer is not healthy",
			zap.String("peer_address", addrStr),
		)

	}
	return allHealthy, healthStatus
}

// Upload compresses, encrypts, and splits the content into pages and then stores them in the
// libri network. It returns the uploaded envelope for self-storage and its key.
func (a *Author) Upload(content io.Reader, mediaType string) (*api.Document, id.ID, error) {
	startTime := time.Now()
	authorPub, readerPub, kek, eek, err := a.envKeys.sample()
	if err != nil {
		return nil, nil, err
	}

	a.logger.Debug("packing content",
		zap.String(LoggerAuthorPub, fmt.Sprintf("%065x", authorPub)),
	)
	entry, metadata, err := a.entryPacker.Pack(content, mediaType, eek, authorPub)
	if err != nil {
		return nil, nil, err
	}

	a.logger.Debug("shipping entry",
		zap.String(LoggerAuthorPub, fmt.Sprintf("%065x", authorPub)),
		zap.String(LoggerReaderPub, fmt.Sprintf("%065x", readerPub)),
	)
	env, envKey, err := a.shipper.ShipEntry(entry, authorPub, readerPub, kek, eek)
	if err != nil {
		return nil, nil, err
	}

	elapsedTime := time.Since(startTime)
	entryKeyBytes := env.Contents.(*api.Document_Envelope).Envelope.EntryKey
	uncompressedSize, _ := metadata.GetUncompressedSize()
	ciphertextSize, _ := metadata.GetCiphertextSize()
	speedMbps := float32(uncompressedSize) * 8 / float32(2<<20) / float32(elapsedTime.Seconds())
	a.logger.Info("successfully uploaded document",
		zap.Stringer(LoggerEnvelopeKey, envKey),
		zap.Stringer(LoggerEntryKey, id.FromBytes(entryKeyBytes)),
		zap.Uint64("original_size", uncompressedSize),
		zap.String("original_size_human", humanize.Bytes(uncompressedSize)),
		zap.Uint64("uploaded_size", ciphertextSize),
		zap.String("uploaded_size_human", humanize.Bytes(ciphertextSize)),
		zap.Float32("speed_Mbps", speedMbps),
	)
	return env, envKey, nil
}

// Download downloads, join, decrypts, and decompressed the content, writing it to a unified output
// content writer.
func (a *Author) Download(content io.Writer, envKey id.ID) error {
	startTime := time.Now()
	a.logger.Debug("receiving entry", zap.String(LoggerEnvelopeKey, envKey.String()))
	entry, keys, err := a.receiver.ReceiveEntry(envKey)
	if err != nil {
		return err
	}
	entryKey, nPages, err := getEntryInfo(entry)
	if err != nil {
		return err
	}

	a.logger.Debug("unpacking content",
		zap.String(LoggerEntryKey, entryKey.String()),
		zap.Int(LoggerNPages, nPages),
	)
	metadata, err := a.entryUnpacker.Unpack(content, entry, keys)
	if err != nil {
		return err
	}

	elapsedTime := time.Since(startTime)
	uncompressedSize, _ := metadata.GetUncompressedSize()
	ciphertextSize, _ := metadata.GetCiphertextSize()
	speedMbps := float32(uncompressedSize) * 8 / float32(2<<20) / float32(elapsedTime.Seconds())
	a.logger.Info("successfully downloaded document",
		zap.Stringer(LoggerEnvelopeKey, envKey),
		zap.Stringer(LoggerEntryKey, entryKey),
		zap.String("downloaded_size", humanize.Bytes(ciphertextSize)),
		zap.String("original_size", humanize.Bytes(uncompressedSize)),
		zap.Float32("speed_Mbps", speedMbps),
	)
	return nil
}

// Share creates and uploads a new envelope with the given reader public key. The new envelope
// has the same entry and entry encryption key as that of envelopeKey.
func (a *Author) Share(envKey id.ID, readerPub *ecdsa.PublicKey) (*api.Document, id.ID, error) {

	env, err := a.receiver.ReceiveEnvelope(envKey)
	if err != nil {
		return nil, nil, err
	}
	eek, err := a.receiver.GetEEK(env)
	if err != nil {
		return nil, nil, err
	}
	authorKey, err := a.authorKeys.Sample()
	if err != nil {
		return nil, nil, err
	}
	kek, err := enc.NewKEK(authorKey.Key(), readerPub)
	if err != nil {
		return nil, nil, err
	}
	entryKey := id.FromBytes(env.EntryKey)
	authKeyBs, readKeyBs := authorKey.PublicKeyBytes(), ecid.ToPublicKeyBytes(readerPub)
	sharedEnv, sharedEnvKey, err := a.shipper.ShipEnvelope(kek, eek, entryKey, authKeyBs, readKeyBs)
	if err != nil {
		return nil, nil, err
	}

	a.logger.Info("successfully shared document",
		zap.Stringer(LoggerEntryKey, entryKey),
		zap.Stringer(LoggerEnvelopeKey, envKey),
	)
	a.logger.Debug("shared with",
		zap.String(LoggerAuthorPub, fmt.Sprintf("%065x", authKeyBs)),
		zap.String(LoggerReaderPub, fmt.Sprintf("%065x", readKeyBs)),
	)
	return sharedEnv, sharedEnvKey, nil
}

func getEntryInfo(entry *api.Document) (id.ID, int, error) {
	entryKey, err := api.GetKey(entry)
	if err != nil {
		return nil, 0, err
	}
	pageKeys, err := api.GetEntryPageKeys(entry)
	if err != nil {
		return nil, 0, err
	}
	if len(pageKeys) > 0 {
		return entryKey, len(pageKeys), nil
	}

	// zero extra pages implies a single page entry
	return entryKey, 1, nil
}
