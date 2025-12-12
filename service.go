package email

import (
	"sync"
	"sync/atomic"

	"github.com/Station-Manager/config"
	"github.com/Station-Manager/errors"
	"github.com/Station-Manager/logging"
	"github.com/Station-Manager/types"
)

type Service struct {
	ConfigService *config.Service  `di.inject:"configservice"`
	LoggerService *logging.Service `di.inject:"loggingservice"`
	Config        *types.EmailConfig

	isInitialized atomic.Bool
	initOnce      sync.Once
}

func (s *Service) Initialize() error {
	const op errors.Op = "email.Service.Initialize"
	if s.isInitialized.Load() {
		return nil
	}

	var initErr error
	s.initOnce.Do(func() {
		if s.LoggerService == nil {
			initErr = errors.New(op).Msg("logger service has not been set/injected")
			return
		}

		if s.Config == nil {
			if s.ConfigService == nil {
				initErr = errors.New(op).Msg("application config has not been set/injected")
				return
			}
		}

		if err := s.validateConfig(op); err != nil {
			initErr = err
			return
		}

		s.isInitialized.Store(true)
	})

	return initErr
}
