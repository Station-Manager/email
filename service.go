package email

import (
	"github.com/Station-Manager/config"
	"github.com/Station-Manager/logging"
)

type Service struct {
	ConfigService *config.Service  `di.inject:"configservice"`
	Logger        *logging.Service `di.inject:"loggingservice"`
}

func (s *Service) Initialize() error {
	return nil
}
