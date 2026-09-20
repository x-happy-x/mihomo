package constant

import "testing"

// fakeAdapter is a ProxyAdapter stand-in; only identity matters here.
type fakeAdapter struct {
	ProxyAdapter
	id string
}

type fakeWrapper struct {
	ProxyAdapter
	inner ProxyAdapter
}

func (w fakeWrapper) InnerProxyAdapter() ProxyAdapter { return w.inner }

func TestUnwrapProxyAdapter(t *testing.T) {
	inner := fakeAdapter{id: "inner"}

	t.Run("plain adapter is returned as is", func(t *testing.T) {
		if got := UnwrapProxyAdapter(inner); got != ProxyAdapter(inner) {
			t.Fatalf("UnwrapProxyAdapter returned %v, want the adapter itself", got)
		}
	})

	t.Run("single wrapper is unwrapped", func(t *testing.T) {
		if got := UnwrapProxyAdapter(fakeWrapper{inner: inner}); got != ProxyAdapter(inner) {
			t.Fatalf("UnwrapProxyAdapter returned %v, want inner", got)
		}
	})

	t.Run("nested wrappers are unwrapped", func(t *testing.T) {
		wrapped := fakeWrapper{inner: fakeWrapper{inner: inner}}
		if got := UnwrapProxyAdapter(wrapped); got != ProxyAdapter(inner) {
			t.Fatalf("UnwrapProxyAdapter returned %v, want inner", got)
		}
	})

	t.Run("wrapper with no inner does not loop forever", func(t *testing.T) {
		wrapper := fakeWrapper{inner: nil}
		if got := UnwrapProxyAdapter(wrapper); got != ProxyAdapter(wrapper) {
			t.Fatalf("UnwrapProxyAdapter returned %v, want the wrapper itself", got)
		}
	})
}
