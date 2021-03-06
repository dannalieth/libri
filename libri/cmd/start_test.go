package cmd

import (
	"testing"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/drausin/libri/libri/librarian/server"
)

func TestGetLibrarianConfig_ok(t *testing.T) {
	localIP, publicIP := "1.2.3.4", "5.6.7.8"
	localPort, publicPort := "1234", "5678"
	publicName := "some name"
	dataDir := "some/data/dir"
	logLevel := "debug"
	nSubscriptions, fpRate := 5, 0.5
	bootstraps := "1.2.3.5:1000 1.2.3.6:1000"

	viper.Set(logLevelFlag, logLevel)
	viper.Set(localHostFlag, localIP)
	viper.Set(publicHostFlag, publicIP)
	viper.Set(localPortFlag, localPort)
	viper.Set(publicPortFlag, publicPort)
	viper.Set(publicNameFlag, publicName)
	viper.Set(dataDirFlag, dataDir)
	viper.Set(nSubscriptionsFlag, nSubscriptions)
	viper.Set(fpRateFlag, fpRate)
	viper.Set(bootstrapsFlag, bootstraps)

	config, logger, err := getLibrarianConfig()
	assert.Nil(t, err)
	assert.NotNil(t, logger)
	assert.Equal(t, localIP + ":" + localPort, config.LocalAddr.String())
	assert.Equal(t, publicIP + ":" + publicPort, config.PublicAddr.String())
	assert.Equal(t, publicName, config.PublicName)
	assert.Equal(t, dataDir, config.DataDir)
	assert.Equal(t, dataDir + "/" + server.DBSubDir, config.DbDir)
	assert.Equal(t, logLevel, config.LogLevel.String())
	assert.Equal(t, uint32(nSubscriptions), config.SubscribeTo.NSubscriptions)
	assert.Equal(t, float32(fpRate), config.SubscribeTo.FPRate)
	assert.Equal(t, 2, len(config.BootstrapAddrs))
}

func TestGetLibrarianConfig_err(t *testing.T) {
	viper.Set(localHostFlag, "bad local host")
	config, logger, err := getLibrarianConfig()
	assert.NotNil(t, err)
	assert.Nil(t, config)
	assert.Nil(t, logger)

	viper.Set(localHostFlag, "1.2.3.4")
	viper.Set(publicHostFlag, "bad public host")
	config, logger, err = getLibrarianConfig()
	assert.NotNil(t, err)
	assert.Nil(t, config)
	assert.Nil(t, logger)

	viper.Set(publicHostFlag, "1.2.3.4")
	viper.Set(bootstrapsFlag, "bad bootstrap")
	config, logger, err = getLibrarianConfig()
	assert.NotNil(t, err)
	assert.Nil(t, config)
	assert.Nil(t, logger)
}