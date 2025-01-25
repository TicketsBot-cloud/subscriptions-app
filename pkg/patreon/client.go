package patreon

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/TicketsBot/subscriptions-app/internal/config"
	"github.com/jackc/pgx/v4/pgxpool"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
)

type Client struct {
	httpClient  *http.Client
	config      config.Config
	logger      *zap.Logger
	ratelimiter *rate.Limiter
	db          *pgxpool.Pool

	Tokens Tokens
}

const UserAgent = "ticketsbot.cloud/subscriptions-app (https://github.com/TicketsBot/subscriptions-app)"

func NewClient(config config.Config, logger *zap.Logger, pool *pgxpool.Pool) *Client {
	return &Client{
		httpClient: http.DefaultClient,
		config:     config,
		logger:     logger,
		ratelimiter: rate.NewLimiter(
			rate.Every(time.Minute/time.Duration(config.Patreon.RequestsPerMinute)),
			config.Patreon.RequestsPerMinute,
		),
		db: pool,
	}
}

func (c *Client) RefreshCredentials(ctx context.Context) error {
	var accessToken, refreshToken string
	var expiresAt time.Time
	if err := c.db.QueryRow(ctx, "SELECT access_token, refresh_token, expires FROM patreon_keys").Scan(&accessToken, &refreshToken, &expiresAt); err != nil {
		c.logger.Error("Invalid access token or refresh token")
		return err
	}

	c.Tokens = Tokens{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt,
	}

	return nil
}

func (c *Client) FetchPledges(ctx context.Context) (map[string]Patron, error) {
	url := fmt.Sprintf(
		"https://www.patreon.com/api/oauth2/v2/campaigns/%d/members?include=currently_entitled_tiers,user&fields%%5Bmember%%5D=last_charge_date,last_charge_status,patron_status,email,pledge_relationship_start&fields%%5Buser%%5D=social_connections",
		c.config.Patreon.CampaignId,
	)

	// Email -> Data
	data := make(map[string]Patron)
	for {
		res, err := c.FetchPageWithTimeout(ctx, 10*time.Minute, url)
		if err != nil {
			return nil, err
		}

		for _, member := range res.Data {
			id := member.Relationships.User.Data.Id

			if member.Attributes.Email == "" {
				c.logger.Debug("member has no email", zap.Uint64("patron_id", id))
				continue
			}

			// Parse tiers
			var tiers []uint64
			for _, tier := range member.Relationships.CurrentlyEntitledTiers.Data {
				// Check if tier is known
				if _, ok := c.config.Tiers[tier.TierId]; !ok {
					c.logger.Warn("unknown tier", zap.Uint64("tier_id", tier.TierId))
					continue
				}

				tiers = append(tiers, tier.TierId)
			}

			// Parse "included" metadata
			var discordId *uint64
			for _, included := range res.Included {
				if id == included.Id {
					if tmp := included.Attributes.SocialConnections.Discord.Id; tmp != nil {
						discordId = tmp
					}

					break
				}
			}

			data[member.Attributes.Email] = Patron{
				Attributes: member.Attributes,
				Id:         id,
				Tiers:      tiers,
				DiscordId:  discordId,
			}
		}

		if res.Links == nil || res.Links.Next == nil {
			break
		}

		url = *res.Links.Next
	}

	return data, nil
}

func (c *Client) FetchPageWithTimeout(ctx context.Context, timeout time.Duration, url string) (PledgeResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return c.FetchPage(ctx, url)
}

func (c *Client) FetchPage(ctx context.Context, url string) (PledgeResponse, error) {
	c.logger.Debug("Fetching page", zap.String("url", url))

	if c.Tokens.ExpiresAt.Before(time.Now()) {
		return PledgeResponse{}, fmt.Errorf("can't refresh: refresh token has already expired (expired at %s)", c.Tokens.ExpiresAt.String())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return PledgeResponse{}, err
	}

	req.Header.Set("Authorization", "Bearer "+c.Tokens.AccessToken)
	req.Header.Set("User-Agent", UserAgent)

	if err := c.ratelimiter.Wait(ctx); err != nil {
		return PledgeResponse{}, err
	}

	res, err := c.httpClient.Do(req)
	if err != nil {
		return PledgeResponse{}, err
	}

	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		body, err := ioutil.ReadAll(res.Body)
		if err != nil {
			c.logger.Error(
				"error reading body of pledge response",
				zap.Int("status_code", res.StatusCode),
				zap.Error(err),
			)
			return PledgeResponse{}, err
		}

		c.logger.Error(
			"pledge response returned non-OK status code",
			zap.Int("status_code", res.StatusCode),
			zap.String("body", string(body)),
		)

		return PledgeResponse{}, fmt.Errorf("pledge response returned %d status code", res.StatusCode)
	}

	var body PledgeResponse
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return PledgeResponse{}, err
	}

	c.logger.Debug("Page fetched successfully", zap.String("url", url))

	return body, nil
}
