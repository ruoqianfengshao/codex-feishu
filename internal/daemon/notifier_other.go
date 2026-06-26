//go:build !darwin

package daemon

func newSystemNotifier() Notifier {
	return noopNotifier{}
}
