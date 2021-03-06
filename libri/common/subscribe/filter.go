package subscribe

import (
	"encoding/gob"
	"math/rand"

	"github.com/drausin/libri/libri/librarian/api"
	"github.com/willf/bloom"
)

var minFilterElements = 10

// ToAPI converts a *bloom.BloomFilter (via narrower gob.GobEncoder) to an *api.BloomFilter.
func ToAPI(f gob.GobEncoder) (*api.BloomFilter, error) {
	encoded, err := f.GobEncode()
	if err != nil {
		// should never happen
		return nil, err
	}
	return &api.BloomFilter{
		Encoded: encoded,
	}, nil
}

// FromAPI converts an *api.BloomFilter to a *bloom.BloomFilter.
func FromAPI(f *api.BloomFilter) (*bloom.BloomFilter, error) {
	decoded := bloom.New(1, 1)
	err := decoded.GobDecode(f.Encoded)
	if err != nil {
		return nil, err
	}
	return decoded, nil
}

func newFilter(elements [][]byte, fp float64, rng *rand.Rand) *bloom.BloomFilter {
	if fp == 1.0 {
		return alwaysInFilter()
	}
	for len(elements) < minFilterElements {
		elements = append(elements, api.RandBytes(rng, api.ECPubKeyLength))
	}
	filter := bloom.NewWithEstimates(uint(len(elements)), fp)
	for _, e := range elements {
		filter.Add(e)
	}
	return filter
}

func alwaysInFilter() *bloom.BloomFilter {
	filter := bloom.New(1, 1)
	filter.Add([]byte{1}) // could be anything
	return filter
}
