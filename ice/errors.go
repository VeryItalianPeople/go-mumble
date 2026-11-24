package ice

import (
	"errors"
	"strings"

	"github.com/VeryItalianPeople/go-mumble/internal/iceproto"
)

// Errors for the exceptions Murmur throws. Test with [errors.Is]; the
// returned error is an [*Error] with the operation and the exception's type
// ID.
var (
	ErrInvalidSession   = errors.New("murmur: invalid session")
	ErrInvalidChannel   = errors.New("murmur: invalid channel")
	ErrInvalidServer    = errors.New("murmur: invalid server")
	ErrInvalidUser      = errors.New("murmur: invalid user")
	ErrInvalidTexture   = errors.New("murmur: invalid texture")
	ErrInvalidCallback  = errors.New("murmur: invalid callback")
	ErrInvalidSecret    = errors.New("murmur: invalid Ice secret")
	ErrInvalidInputData = errors.New("murmur: invalid input data")
	ErrNestingLimit     = errors.New("murmur: channel nesting limit reached")
	ErrWriteOnly        = errors.New("murmur: configuration key is write-only")
	ErrServerFailure    = errors.New("murmur: server failed to start")
	// ErrServerBooted is what Murmur throws both for starting a server that
	// is running and for using one that is not: most operations on a
	// stopped virtual server fail with it.
	ErrServerBooted = errors.New("murmur: server booted state is wrong for this call")
	// ErrInvalidListener, ErrReadOnlyMode and ErrInternal only exist in
	// Mumble 1.6.
	ErrInvalidListener = errors.New("murmur: invalid listener")
	ErrReadOnlyMode    = errors.New("murmur: database is in read-only mode")
	ErrInternal        = errors.New("murmur: internal error")
	// ErrServerNotFound means the virtual server doesn't exist.
	ErrServerNotFound = errors.New("murmur: no such virtual server")
	// ErrUnsupported means the operation doesn't exist on this Murmur,
	// e.g. the database state calls on 1.5.
	ErrUnsupported = errors.New("murmur: operation not supported by this Murmur")
	// ErrWouldCrash is returned, without making the call, for an
	// operation that crashes this version of Murmur in the current
	// situation. See [Server.Conf] and [Server.SetTexture].
	ErrWouldCrash = errors.New("murmur: refusing a call that crashes this Murmur")
)

var exceptionErrors = map[string]error{
	"InvalidSessionException":   ErrInvalidSession,
	"InvalidChannelException":   ErrInvalidChannel,
	"InvalidServerException":    ErrInvalidServer,
	"InvalidUserException":      ErrInvalidUser,
	"InvalidTextureException":   ErrInvalidTexture,
	"InvalidCallbackException":  ErrInvalidCallback,
	"InvalidSecretException":    ErrInvalidSecret,
	"InvalidInputDataException": ErrInvalidInputData,
	"NestingLimitException":     ErrNestingLimit,
	"WriteOnlyException":        ErrWriteOnly,
	"ServerFailureException":    ErrServerFailure,
	"ServerBootedException":     ErrServerBooted,
	"InvalidListenerException":  ErrInvalidListener,
	"ReadOnlyModeException":     ErrReadOnlyMode,
	"InternalErrorException":    ErrInternal,
}

// Error is a failed operation.
type Error struct {
	// Operation is the Ice operation, e.g. "getChannelState".
	Operation string
	// TypeID is the Slice type ID of the exception Murmur threw, e.g.
	// "::MumbleServer::InvalidChannelException", or empty for failures of
	// the Ice runtime itself.
	TypeID string
	// Err is the underlying Ice error.
	Err error

	sentinel error
}

func (e *Error) Error() string {
	if e.sentinel != nil {
		return e.sentinel.Error() + " (" + e.Operation + ")"
	}
	return "murmur: " + e.Operation + ": " + e.Err.Error()
}

// Is matches the Err* sentinel for the exception.
func (e *Error) Is(target error) bool { return e.sentinel != nil && e.sentinel == target }

// Unwrap returns the underlying Ice error.
func (e *Error) Unwrap() error { return e.Err }

func wrapError(op string, err error) error {
	if err == nil {
		return nil
	}
	var ue *iceproto.UserException
	if errors.As(err, &ue) {
		e := &Error{Operation: op, TypeID: ue.TypeID, Err: err}
		name := ue.TypeID[strings.LastIndex(ue.TypeID, ":")+1:]
		e.sentinel = exceptionErrors[name]
		return e
	}
	var de *iceproto.DispatchError
	if errors.As(err, &de) {
		e := &Error{Operation: op, Err: err}
		switch {
		case de.Status == iceproto.ObjectNotExist && de.Identity.Category == "s":
			e.sentinel = ErrServerNotFound
		case de.Status == iceproto.OperationNotExist:
			e.sentinel = ErrUnsupported
		}
		return e
	}
	return err
}
