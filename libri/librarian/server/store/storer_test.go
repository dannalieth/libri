package store

import (
	"math/rand"
	"testing"

	"errors"

	"github.com/drausin/libri/libri/common/ecid"
	cid "github.com/drausin/libri/libri/common/id"
	"github.com/drausin/libri/libri/librarian/api"
	"github.com/drausin/libri/libri/librarian/client"
	"github.com/drausin/libri/libri/librarian/server/peer"
	ssearch "github.com/drausin/libri/libri/librarian/server/search"
	"github.com/stretchr/testify/assert"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

func TestNewDefaultStorer(t *testing.T) {
	rng := rand.New(rand.NewSource(0))
	s := NewDefaultStorer(ecid.NewPseudoRandom(rng))
	assert.NotNil(t, s.(*storer).signer)
	assert.NotNil(t, s.(*storer).searcher)
	assert.NotNil(t, s.(*storer).querier)
}

// TestStoreQuerier mocks the StoreQuerier interface. The Query() method returns an
// api.StoreResponse, as if the remote peer had successfully stored the value.
type TestStoreQuerier struct {
	peerID ecid.ID
}

func (c *TestStoreQuerier) Query(ctx context.Context, pConn api.Connector, rq *api.StoreRequest,
	opts ...grpc.CallOption) (*api.StoreResponse, error) {
	return &api.StoreResponse{
		Metadata: &api.ResponseMetadata{
			RequestId: rq.Metadata.RequestId,
			PubKey:    c.peerID.PublicKeyBytes(),
		},
	}, nil
}

func NewTestStorer(peerID ecid.ID, peersMap map[string]peer.Peer) Storer {
	return &storer{
		searcher: ssearch.NewTestSearcher(peersMap),
		querier:  &TestStoreQuerier{peerID: peerID},
		signer:   &client.TestNoOpSigner{},
	}
}

func TestStorer_Store_ok(t *testing.T) {
	n, nReplicas := 32, uint(3)
	rng := rand.New(rand.NewSource(int64(n)))
	peers, peersMap, selfPeerIdxs, selfID := ssearch.NewTestPeers(rng, n)

	// create our searcher
	value, key := api.NewTestDocument(rng)
	storer := NewTestStorer(selfID, peersMap)

	for concurrency := uint(1); concurrency <= 3; concurrency++ {

		searchParams := &ssearch.Parameters{
			NMaxErrors:        DefaultNMaxErrors,
			Concurrency:       concurrency,
			Timeout:           DefaultQueryTimeout,
		}
		storeParams := &Parameters{
			NReplicas: nReplicas,
			NMaxErrors:  DefaultNMaxErrors,
			Concurrency: concurrency,
		}
		store := NewStore(selfID, key, value, searchParams, storeParams)

		// init the seeds of our search: usually this comes from the routing.Table.Peak()
		// method, but we'll just allocate directly
		seeds := make([]peer.Peer, len(selfPeerIdxs))
		for i := 0; i < len(selfPeerIdxs); i++ {
			seeds[i] = peers[selfPeerIdxs[i]]
		}

		// do the search!
		err := storer.Store(store, seeds)

		// checks
		assert.Nil(t, err)
		assert.True(t, store.Finished())
		assert.True(t, store.Stored())
		assert.False(t, store.Errored())

		assert.True(t, uint(len(store.Result.Responded)) >= nReplicas)
		assert.True(t, uint(len(store.Result.Unqueried)) <= storeParams.NMaxErrors)
		assert.Equal(t, 0, len(store.Result.Errors))
		assert.Nil(t, store.Result.FatalErr)
	}
}

type fixedSearcher struct {
	fixed *ssearch.Result
}

func (s *fixedSearcher) Search(search *ssearch.Search, seeds []peer.Peer) error {
	search.Result = s.fixed
	return nil
}

func TestStorer_Store_queryErr(t *testing.T) {
	storerImpl, store, selfPeerIdxs, peers, _ := newTestStore()
	seeds := ssearch.NewTestSeeds(peers, selfPeerIdxs)

	// mock querier to always timeout
	storerImpl.(*storer).querier = &timeoutQuerier{}

	// do the search!
	err := storerImpl.Store(store, seeds)

	// checks
	assert.Nil(t, err)
	assert.True(t, store.Errored())    // since all of the queries return errors
	assert.False(t, store.Exhausted()) // since NMaxErrors < len(Unqueried)
	assert.False(t, store.Stored())
	assert.True(t, store.Finished())

	assert.Equal(t, 0, len(store.Result.Responded))
	assert.True(t, 0 < len(store.Result.Unqueried))
	assert.Equal(t, int(store.Params.NMaxErrors), len(store.Result.Errors))
	assert.Nil(t, store.Result.FatalErr)
}

func newTestStore() (Storer, *Store, []int, []peer.Peer, cid.ID) {
	n := 32
	rng := rand.New(rand.NewSource(int64(n)))
	peers, peersMap, selfPeerIdxs, selfID := ssearch.NewTestPeers(rng, n)

	// create our searcher
	value, key := api.NewTestDocument(rng)
	storerImpl := NewTestStorer(selfID, peersMap)

	concurrency := uint(1)
	searchParams := &ssearch.Parameters{
		NMaxErrors:        ssearch.DefaultNMaxErrors,
		Concurrency:       concurrency,
		Timeout:           DefaultQueryTimeout,
	}
	storeParams := &Parameters{
		NReplicas: DefaultNReplicas,
		NMaxErrors:  DefaultNMaxErrors,
		Concurrency: concurrency,
	}
	store := NewStore(selfID, key, value, searchParams, storeParams)

	return storerImpl, store, selfPeerIdxs, peers, key
}

type errSearcher struct{}

func (es *errSearcher) Search(search *ssearch.Search, seeds []peer.Peer) error {
	return errors.New("some search error")
}

func TestStorer_Store_err(t *testing.T) {
	s := &storer{
		searcher: &errSearcher{},
	}

	// check that Store() surfaces searcher error
	store := &Store{
		Result: &Result{},
	}
	assert.NotNil(t, s.Store(store, nil))
}

// timeoutQuerier returns an error simulating a request timeout
type timeoutQuerier struct{}

func (f *timeoutQuerier) Query(ctx context.Context, pConn api.Connector, fr *api.StoreRequest,
	opts ...grpc.CallOption) (*api.StoreResponse, error) {
	return nil, errors.New("simulated timeout error")
}

// diffRequestIDFinder returns a response with a different request ID
type diffRequestIDQuerier struct {
	rng    *rand.Rand
	peerID ecid.ID
}

func (f *diffRequestIDQuerier) Query(ctx context.Context, pConn api.Connector,
	fr *api.StoreRequest, opts ...grpc.CallOption) (*api.StoreResponse, error) {
	return &api.StoreResponse{
		Metadata: &api.ResponseMetadata{
			RequestId: cid.NewPseudoRandom(f.rng).Bytes(),
			PubKey:    f.peerID.PublicKeyBytes(),
		},
	}, nil
}

func TestStorer_query_err(t *testing.T) {
	rng := rand.New(rand.NewSource(int64(0)))
	clientConn := api.NewConnector(nil) // won't actually be used since we're mocking the finder
	value, key := api.NewTestDocument(rng)
	selfID := ecid.NewPseudoRandom(rng)
	searchParams := &ssearch.Parameters{Timeout: DefaultQueryTimeout}
	store := NewStore(selfID, key, value, searchParams, &Parameters{})

	s1 := &storer{
		signer: &client.TestNoOpSigner{},
		// use querier that simulates a timeout
		querier: &timeoutQuerier{},
	}
	rp1, err := s1.query(clientConn, store)
	assert.Nil(t, rp1)
	assert.NotNil(t, err)

	s2 := &storer{
		signer: &client.TestNoOpSigner{},
		// use querier that simulates a different request ID
		querier: &diffRequestIDQuerier{
			rng:    rng,
			peerID: selfID,
		},
	}
	rp2, err := s2.query(clientConn, store)
	assert.Nil(t, rp2)
	assert.NotNil(t, err)

	s3 := &storer{
		// use signer that returns an error
		signer: &client.TestErrSigner{},
	}
	rp3, err := s3.query(clientConn, store)
	assert.Nil(t, rp3)
	assert.NotNil(t, err)
}
