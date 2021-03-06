package manager

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/lbryio/lbry.go/v2/extras/errors"
	"github.com/lbryio/lbry.go/v2/extras/jsonrpc"
	"github.com/lbryio/lbry.go/v2/extras/util"
	logUtils "github.com/lbryio/ytsync/util"

	"github.com/lbryio/ytsync/tags_manager"
	"github.com/lbryio/ytsync/thumbs"

	"github.com/shopspring/decimal"
	log "github.com/sirupsen/logrus"
	"google.golang.org/api/googleapi/transport"
	"google.golang.org/api/youtube/v3"
)

func (s *Sync) enableAddressReuse() error {
	accountsResponse, err := s.daemon.AccountList(1, 50)
	if err != nil {
		return errors.Err(err)
	}
	accounts := make([]jsonrpc.Account, 0, len(accountsResponse.Items))
	ledger := "lbc_mainnet"
	if logUtils.IsRegTest() {
		ledger = "lbc_regtest"
	}
	for _, a := range accountsResponse.Items {
		if *a.Ledger == ledger {
			accounts = append(accounts, a)
		}
	}

	for _, a := range accounts {
		_, err = s.daemon.AccountSet(a.ID, jsonrpc.AccountSettings{
			ChangeMaxUses:    util.PtrToInt(1000),
			ReceivingMaxUses: util.PtrToInt(100),
		})
		if err != nil {
			return errors.Err(err)
		}
	}
	return nil
}
func (s *Sync) walletSetup() error {
	//prevent unnecessary concurrent execution and publishing while refilling/reallocating UTXOs
	s.walletMux.Lock()
	defer s.walletMux.Unlock()
	err := s.ensureChannelOwnership()
	if err != nil {
		return err
	}

	balanceResp, err := s.daemon.AccountBalance(nil)
	if err != nil {
		return err
	} else if balanceResp == nil {
		return errors.Err("no response")
	}
	balance, err := strconv.ParseFloat(balanceResp.Available.String(), 64)
	if err != nil {
		return errors.Err(err)
	}
	log.Debugf("Starting balance is %.4f", balance)

	n, err := s.CountVideos()
	if err != nil {
		return err
	}
	videosOnYoutube := int(n)

	log.Debugf("Source channel has %d videos", videosOnYoutube)
	if videosOnYoutube == 0 {
		return nil
	}

	s.syncedVideosMux.RLock()
	publishedCount := 0
	notUpgradedCount := 0
	failedCount := 0
	for _, sv := range s.syncedVideos {
		if sv.Published {
			publishedCount++
			if sv.MetadataVersion < 2 {
				notUpgradedCount++
			}
		} else {
			failedCount++
		}
	}
	s.syncedVideosMux.RUnlock()

	log.Debugf("We already allocated credits for %d published videos and %d failed videos", publishedCount, failedCount)

	if videosOnYoutube > s.Manager.videosLimit {
		videosOnYoutube = s.Manager.videosLimit
	}
	unallocatedVideos := videosOnYoutube - (publishedCount + failedCount)
	channelFee := channelClaimAmount
	channelAlreadyClaimed := s.lbryChannelID != ""
	if channelAlreadyClaimed {
		channelFee = 0.0
	}
	requiredBalance := float64(unallocatedVideos)*(publishAmount+estimatedMaxTxFee) + channelFee
	if s.Manager.SyncFlags.UpgradeMetadata {
		requiredBalance += float64(notUpgradedCount) * 0.001
	}

	refillAmount := 0.0
	if balance < requiredBalance || balance < minimumAccountBalance {
		refillAmount = math.Max(math.Max(requiredBalance-balance, minimumAccountBalance-balance), minimumRefillAmount)
	}

	if s.Refill > 0 {
		refillAmount += float64(s.Refill)
	}

	if refillAmount > 0 {
		err := s.addCredits(refillAmount)
		if err != nil {
			return errors.Err(err)
		}
	}

	claimAddress, err := s.daemon.AddressList(nil, nil, 1, 20)
	if err != nil {
		return err
	} else if claimAddress == nil {
		return errors.Err("could not get an address")
	}
	s.claimAddress = string(claimAddress.Items[0].Address)
	if s.claimAddress == "" {
		return errors.Err("found blank claim address")
	}
	if s.shouldTransfer() {
		s.claimAddress = s.clientPublishAddress
	}

	err = s.ensureEnoughUTXOs()
	if err != nil {
		return err
	}

	return nil
}

func (s *Sync) getDefaultAccount() (string, error) {
	if s.defaultAccountID == "" {
		accountsResponse, err := s.daemon.AccountList(1, 50)
		if err != nil {
			return "", errors.Err(err)
		}
		ledger := "lbc_mainnet"
		if logUtils.IsRegTest() {
			ledger = "lbc_regtest"
		}
		for _, a := range accountsResponse.Items {
			if *a.Ledger == ledger {
				if a.IsDefault {
					s.defaultAccountID = a.ID
					break
				}
			}
		}

		if s.defaultAccountID == "" {
			return "", errors.Err("No default account found")
		}
	}
	return s.defaultAccountID, nil
}

func (s *Sync) ensureEnoughUTXOs() error {
	defaultAccount, err := s.getDefaultAccount()
	if err != nil {
		return err
	}

	utxolist, err := s.daemon.UTXOList(&defaultAccount, 1, 10000)
	if err != nil {
		return err
	} else if utxolist == nil {
		return errors.Err("no response")
	}

	target := 40
	slack := int(float32(0.1) * float32(target))
	count := 0
	confirmedCount := 0

	for _, utxo := range utxolist.Items {
		amount, _ := strconv.ParseFloat(utxo.Amount, 64)
		if utxo.IsMine && utxo.Type == "payment" && amount > 0.001 {
			if utxo.Confirmations > 0 {
				confirmedCount++
			}
			count++
		}
	}
	log.Infof("utxo count: %d (%d confirmed)", count, confirmedCount)
	UTXOWaitThreshold := 16
	if count < target-slack {
		balance, err := s.daemon.AccountBalance(&defaultAccount)
		if err != nil {
			return err
		} else if balance == nil {
			return errors.Err("no response")
		}

		balanceAmount, err := strconv.ParseFloat(balance.Available.String(), 64)
		if err != nil {
			return errors.Err(err)
		}
		maxUTXOs := uint64(500)
		desiredUTXOCount := uint64(math.Floor((balanceAmount) / 0.1))
		if desiredUTXOCount > maxUTXOs {
			desiredUTXOCount = maxUTXOs
		}
		availableBalance, _ := balance.Available.Float64()
		log.Infof("Splitting balance of %.3f evenly between %d UTXOs", availableBalance, desiredUTXOCount)

		broadcastFee := 0.1
		prefillTx, err := s.daemon.AccountFund(defaultAccount, defaultAccount, fmt.Sprintf("%.4f", balanceAmount-broadcastFee), desiredUTXOCount, false)
		if err != nil {
			return err
		} else if prefillTx == nil {
			return errors.Err("no response")
		}
		if confirmedCount < UTXOWaitThreshold {
			err = s.waitForNewBlock()
			if err != nil {
				return err
			}
		}
	} else if confirmedCount < UTXOWaitThreshold {
		log.Println("Waiting for previous txns to confirm")
		err := s.waitForNewBlock()
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *Sync) waitForNewBlock() error {
	log.Printf("regtest: %t, docker: %t", logUtils.IsRegTest(), logUtils.IsUsingDocker())
	status, err := s.daemon.Status()
	if err != nil {
		return err
	}
	for status.Wallet.Blocks == 0 || status.Wallet.BlocksBehind != 0 {
		time.Sleep(5 * time.Second)
		status, err = s.daemon.Status()
		if err != nil {
			return err
		}
	}

	currentBlock := status.Wallet.Blocks
	for i := 0; status.Wallet.Blocks <= currentBlock; i++ {
		if i%3 == 0 {
			log.Printf("Waiting for new block (%d)...", currentBlock+1)
		}
		if logUtils.IsRegTest() && logUtils.IsUsingDocker() {
			err = s.GenerateRegtestBlock()
			if err != nil {
				return err
			}
		}
		time.Sleep(10 * time.Second)
		status, err = s.daemon.Status()
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Sync) GenerateRegtestBlock() error {
	lbrycrd, err := logUtils.GetLbrycrdClient(s.LbrycrdString)
	if err != nil {
		return errors.Prefix("error getting lbrycrd client: ", err)
	}
	txs, err := lbrycrd.Generate(1)
	if err != nil {
		return errors.Prefix("error generating new block: ", err)
	}
	for _, tx := range txs {
		log.Info("Generated tx: ", tx.String())
	}
	return nil
}

func (s *Sync) ensureChannelOwnership() error {
	if s.LbryChannelName == "" {
		return errors.Err("no channel name set")
	}

	channels, err := s.daemon.ChannelList(nil, 1, 50, nil)
	if err != nil {
		return err
	} else if channels == nil {
		return errors.Err("no channel response")
	}

	var channelToUse *jsonrpc.Transaction
	if len((*channels).Items) > 0 {
		if s.lbryChannelID == "" {
			return errors.Err("this channel does not have a recorded claimID in the database. To prevent failures, updates are not supported until an entry is manually added in the database")
		}
		for _, c := range (*channels).Items {
			log.Debugf("checking listed channel %s (%s)", c.ClaimID, c.Name)
			if c.ClaimID != s.lbryChannelID {
				continue
			}
			if c.Name != s.LbryChannelName {
				return errors.Err("the channel in the wallet is different than the channel in the database")
			}
			channelToUse = &c
			break
		}
		if channelToUse == nil {
			return errors.Err("this wallet has channels but not a single one is ours! Expected claim_id: %s (%s)", s.lbryChannelID, s.LbryChannelName)
		}
	} else if s.transferState == TransferStateComplete {
		return errors.Err("the channel was transferred but appears to have been abandoned!")
	} else if s.lbryChannelID != "" {
		return errors.Err("the database has a channel recorded (%s) but nothing was found in our control", s.lbryChannelID)
	}

	channelUsesOldMetadata := false
	if channelToUse != nil {
		channelUsesOldMetadata = channelToUse.Value.GetThumbnail() == nil
		if !channelUsesOldMetadata {
			return nil
		}
	}

	channelBidAmount := channelClaimAmount

	balanceResp, err := s.daemon.AccountBalance(nil)
	if err != nil {
		return err
	} else if balanceResp == nil {
		return errors.Err("no response")
	}
	balance, err := decimal.NewFromString(balanceResp.Available.String())
	if err != nil {
		return errors.Err(err)
	}

	if balance.LessThan(decimal.NewFromFloat(channelBidAmount)) {
		err = s.addCredits(channelBidAmount + 0.3)
		if err != nil {
			return err
		}
	}
	client := &http.Client{
		Transport: &transport.APIKey{Key: s.APIConfig.YoutubeAPIKey},
	}

	service, err := youtube.New(client)
	if err != nil {
		return errors.Prefix("error creating YouTube service", err)
	}

	response, err := service.Channels.List("snippet,brandingSettings").Id(s.YoutubeChannelID).Do()
	if err != nil {
		return errors.Prefix("error getting channel details", err)
	}

	if len(response.Items) < 1 {
		return errors.Err("youtube channel not found")
	}

	channelInfo := response.Items[0].Snippet
	channelBranding := response.Items[0].BrandingSettings

	thumbnail := thumbs.GetBestThumbnail(channelInfo.Thumbnails)
	thumbnailURL, err := thumbs.MirrorThumbnail(thumbnail.Url, s.YoutubeChannelID, s.Manager.GetS3AWSConfig())
	if err != nil {
		return err
	}

	var bannerURL *string
	if channelBranding.Image != nil && channelBranding.Image.BannerImageUrl != "" {
		bURL, err := thumbs.MirrorThumbnail(channelBranding.Image.BannerImageUrl, "banner-"+s.YoutubeChannelID, s.Manager.GetS3AWSConfig())
		if err != nil {
			return err
		}
		bannerURL = &bURL
	}

	var languages []string = nil
	if channelInfo.DefaultLanguage != "" {
		if channelInfo.DefaultLanguage == "iw" {
			channelInfo.DefaultLanguage = "he"
		}
		languages = []string{channelInfo.DefaultLanguage}
	}
	var locations []jsonrpc.Location = nil
	if channelInfo.Country != "" {
		locations = []jsonrpc.Location{{Country: util.PtrToString(channelInfo.Country)}}
	}
	var c *jsonrpc.TransactionSummary
	claimCreateOptions := jsonrpc.ClaimCreateOptions{
		Title:        &channelInfo.Title,
		Description:  &channelInfo.Description,
		Tags:         tags_manager.GetTagsForChannel(s.YoutubeChannelID),
		Languages:    languages,
		Locations:    locations,
		ThumbnailURL: &thumbnailURL,
	}
	if channelUsesOldMetadata {
		c, err = s.daemon.ChannelUpdate(s.lbryChannelID, jsonrpc.ChannelUpdateOptions{
			ClearTags:      util.PtrToBool(true),
			ClearLocations: util.PtrToBool(true),
			ClearLanguages: util.PtrToBool(true),
			ChannelCreateOptions: jsonrpc.ChannelCreateOptions{
				ClaimCreateOptions: claimCreateOptions,
				CoverURL:           bannerURL,
			},
		})
	} else {
		c, err = s.daemon.ChannelCreate(s.LbryChannelName, channelBidAmount, jsonrpc.ChannelCreateOptions{
			ClaimCreateOptions: claimCreateOptions,
			CoverURL:           bannerURL,
		})
	}

	if err != nil {
		return err
	}
	s.lbryChannelID = c.Outputs[0].ClaimID
	return s.Manager.apiConfig.SetChannelClaimID(s.YoutubeChannelID, s.lbryChannelID)
}

func (s *Sync) addCredits(amountToAdd float64) error {
	log.Printf("Adding %f credits", amountToAdd)
	lbrycrdd, err := logUtils.GetLbrycrdClient(s.LbrycrdString)
	if err != nil {
		return err
	}

	defaultAccount, err := s.getDefaultAccount()
	if err != nil {
		return err
	}
	addressResp, err := s.daemon.AddressUnused(&defaultAccount)
	if err != nil {
		return err
	} else if addressResp == nil {
		return errors.Err("no response")
	}
	address := string(*addressResp)

	_, err = lbrycrdd.SimpleSend(address, amountToAdd)
	if err != nil {
		return err
	}

	wait := 15 * time.Second
	log.Println("Waiting " + wait.String() + " for lbryum to let us know we have the new transaction")
	time.Sleep(wait)

	return nil
}
