package policy

import (
	"testing"

	"github.com/ipni/storetheindex/config"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/require"
)

const (
	exceptIDStr = "12D3KooWK7CTS7cyWi51PeNE3cTjS2F2kDCZaQVU4A5xBmb9J1do"
	otherIDStr  = "12D3KooWSG3JuvEjRkSxt93ADTjQxqe4ExbBwSkQ9Zyk1WfBaZJF"
)

var (
	exceptID peer.ID
	otherID  peer.ID
)

func init() {
	var err error
	exceptID, err = peer.Decode(exceptIDStr)
	if err != nil {
		panic(err)
	}
	otherID, err = peer.Decode(otherIDStr)
	if err != nil {
		panic(err)
	}
}

func TestNewPolicy(t *testing.T) {
	policyCfg := config.Policy{
		Allow:         false,
		Except:        []string{exceptIDStr},
		Publish:       false,
		PublishExcept: []string{exceptIDStr},
	}

	_, err := New(policyCfg)
	require.NoError(t, err)

	policyCfg.Allow = true
	_, err = New(policyCfg)
	require.NoError(t, err)

	policyCfg.Allow = false
	policyCfg.PublishExcept = append(policyCfg.PublishExcept, "bad ID")
	_, err = New(policyCfg)
	require.Error(t, err, "expected error with bad PublishExcept ID")
	policyCfg.PublishExcept = nil

	policyCfg.Except = append(policyCfg.Except, "bad ID")
	_, err = New(policyCfg)
	require.Error(t, err, "expected error with bad except ID")
	policyCfg.Except = nil

	_, err = New(policyCfg)
	require.NoError(t, err)

	policyCfg.Allow = true
	_, err = New(policyCfg)
	require.NoError(t, err)
}

func TestPolicyAccess(t *testing.T) {
	policyCfg := config.Policy{
		Allow:         false,
		Except:        []string{exceptIDStr},
		Publish:       false,
		PublishExcept: []string{exceptIDStr},
	}

	p, err := New(policyCfg)
	require.NoError(t, err)

	require.False(t, p.Allowed(otherID), "peer ID should not be allowed by policy")
	require.True(t, p.Allowed(exceptID), "peer ID should be allowed")

	require.False(t, p.PublishAllowed(otherID, exceptID), "peer ID should not be allowed to publish")
	require.True(t, p.PublishAllowed(otherID, otherID), "should be allowed to publish to self")
	require.False(t, p.PublishAllowed(exceptID, otherID), "should not be allowed to publish to blocked peer")

	p.Allow(otherID)
	require.True(t, p.Allowed(otherID), "peer ID should be allowed by policy")

	p.Block(exceptID)
	require.False(t, p.Allowed(exceptID), "peer ID should not be allowed")

	policyCfg.Allow = true
	policyCfg.Publish = true

	newPol, err := New(policyCfg)
	require.NoError(t, err)
	p.Copy(newPol)

	require.True(t, p.Allowed(otherID), "peer ID should be allowed by policy")
	require.False(t, p.Allowed(exceptID), "peer ID should not be allowed")

	require.False(t, p.PublishAllowed(otherID, exceptID), "should not be allowed to publish to blocked peer")
	require.False(t, p.PublishAllowed(exceptID, otherID), "peer ID should not be allowed to publish")
	require.True(t, p.PublishAllowed(exceptID, exceptID), "peer ID be allowed to publish to self")

	p.Allow(exceptID)
	require.True(t, p.Allowed(exceptID), "peer ID should be allowed by policy")

	p.Block(otherID)
	require.False(t, p.Allowed(otherID), "peer ID should not be allowed")

	cfg := p.ToConfig()
	require.True(t, cfg.Allow)
	require.True(t, cfg.Publish)
	require.Equal(t, 1, len(cfg.Except))
	require.Equal(t, otherIDStr, cfg.Except[0])
	require.Equal(t, 1, len(cfg.PublishExcept))

	p, err = New(config.Policy{})
	require.NoError(t, err)
	require.True(t, p.NoneAllowed(), "expected inaccessible policy")
}
