package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/photoprism/photoprism/internal/entity"
	"github.com/photoprism/photoprism/internal/event"
	"github.com/photoprism/photoprism/internal/photoprism/get"
	"github.com/photoprism/photoprism/internal/server/limiter"
	"github.com/photoprism/photoprism/internal/thumb/avatar"
	"github.com/photoprism/photoprism/pkg/authn"
	"github.com/photoprism/photoprism/pkg/clean"
	"github.com/photoprism/photoprism/pkg/header"
	"github.com/photoprism/photoprism/pkg/i18n"
	"github.com/photoprism/photoprism/pkg/txt"
	"github.com/photoprism/photoprism/pkg/unix"
)

// OIDCRedirect creates a new access token when a user has been successfully authenticated,
// and then redirects the browser back to the app.
//
// GET /api/v1/oidc/redirect
func OIDCRedirect(router *gin.RouterGroup) {
	router.GET("/oidc/redirect", func(c *gin.Context) {
		// Get global config.
		conf := get.Config()

		// Prevent CDNs from caching this endpoint.
		if header.IsCdn(c.Request) {
			c.Redirect(http.StatusTemporaryRedirect, conf.LoginUri())
			return
		}

		// Disable caching of responses.
		c.Header(header.CacheControl, header.CacheControlNoStore)

		// Get client IP address for logs and rate limiting checks.
		clientIp := ClientIP(c)
		actor := "unknown user"
		action := "sign in"

		// Abort in public mode and if OIDC is disabled.
		if get.Config().Public() {
			event.AuditErr([]string{clientIp, "oidc", actor, action, authn.ErrDisabledInPublicMode.Error()})
			c.Redirect(http.StatusTemporaryRedirect, conf.LoginUri())
			return
		} else if !conf.OIDCEnabled() {
			event.AuditErr([]string{clientIp, "oidc", actor, action, authn.ErrAuthenticationDisabled.Error()})
			c.Redirect(http.StatusTemporaryRedirect, conf.LoginUri())
			return
		}

		// Check request rate limit.
		var r *limiter.Request
		r = limiter.Login.Request(clientIp)

		// Abort if failure rate limit is exceeded.
		if r.Reject() || limiter.Auth.Reject(clientIp) {
			c.HTML(http.StatusTooManyRequests, "auth.gohtml", CreateSessionError(http.StatusTooManyRequests, i18n.Error(i18n.ErrForbidden)))
			return
		}

		// Check if the required request parameters are present.
		if c.Query("state") == "" || c.Query("code") == "" {
			event.AuditErr([]string{clientIp, "oidc", actor, action, authn.ErrAuthCodeRequired.Error()})
			c.Redirect(http.StatusTemporaryRedirect, conf.LoginUri())
			return
		}

		// Get OIDC provider.
		provider := get.OIDC()

		if provider == nil {
			event.AuditErr([]string{clientIp, "oidc", actor, action, authn.ErrAuthenticationDisabled.Error()})
			c.HTML(http.StatusUnauthorized, "auth.gohtml", CreateSessionError(http.StatusUnauthorized, i18n.Error(i18n.ErrInvalidCredentials)))
			return
		}

		userInfo, tokens, claimErr := provider.CodeExchangeUserInfo(c)

		if claimErr != nil {
			event.AuditErr([]string{clientIp, "oidc", actor, action, claimErr.Error()})
			return
		}

		// Step 1: Create user account if it does not exist yet.
		var user *entity.User
		var err error

		// Find existing user record and update it, if necessary.
		if oidcUser := entity.OidcUser(userInfo, conf.OIDCUsername()); oidcUser.UserName == "" || authn.ProviderOIDC.NotEqual(oidcUser.AuthProvider) {
			event.AuditErr([]string{clientIp, "oidc", actor, action, authn.ErrInvalidUsername.Error()})
			c.HTML(http.StatusUnauthorized, "auth.gohtml", CreateSessionError(http.StatusUnauthorized, i18n.Error(i18n.ErrInvalidCredentials)))
			return
		} else if user = entity.FindUser(oidcUser); user != nil {
			// Check if username and subject UID match.
			if user.Username() == "" || oidcUser.UserName == "" || user.Username() != oidcUser.UserName {
				event.AuditErr([]string{clientIp, "oidc", actor, action, authn.ErrInvalidUsername.Error()})
				c.HTML(http.StatusUnauthorized, "auth.gohtml", CreateSessionError(http.StatusUnauthorized, i18n.Error(i18n.ErrInvalidCredentials)))
				return
			} else if user.AuthID == "" || oidcUser.AuthID == "" || user.AuthID != oidcUser.AuthID {
				event.AuditErr([]string{clientIp, "oidc", actor, action, authn.ErrInvalidAuthID.Error()})
				c.HTML(http.StatusUnauthorized, "auth.gohtml", CreateSessionError(http.StatusUnauthorized, i18n.Error(i18n.ErrInvalidCredentials)))
				return
			}

			actor = user.Username()

			// Update user profile information.
			details := user.Details()

			// Update user display name.
			if entity.SrcPriority[details.NameSrc] <= entity.SrcPriority[entity.SrcOIDC] {
				user.SetDisplayName(userInfo.GetName(), entity.SrcOIDC)
				user.SetGivenName(userInfo.GetGivenName())
				user.SetFamilyName(userInfo.GetFamilyName())
				details.UserGender = clean.Name(string(userInfo.GetGender()))
			}

			// Update nickname.
			if name := clean.Name(userInfo.GetNickname()); name != "" {
				details.NickName = clean.Name(userInfo.GetNickname())
			}

			// Update profile URL.
			if u := clean.Uri(userInfo.GetProfile()); u != "" {
				details.ProfileURL = u
			}

			// Update website URL.
			if u := clean.Uri(userInfo.GetWebsite()); u != "" {
				details.SiteURL = u
			}

			// Update UI locale.
			user.Settings().UILanguage = clean.Locale(userInfo.GetLocale().String(), user.Settings().UILanguage)

			// Update UI timezone.
			if tz := userInfo.GetZoneinfo(); tz != "" && tz != time.UTC.String() {
				user.Settings().UITimeZone = tz
			}

			// Update user location, if available.
			if addr := userInfo.GetAddress(); addr != nil {
				user.Details().UserLocation = clean.Name(addr.GetLocality())
				user.Details().UserCountry = clean.TypeLowerUnderscore(addr.GetCountry())
			}

			// Update birthday, if available.
			if birthDate := txt.ParseTime(userInfo.GetBirthdate(), userInfo.GetZoneinfo()); !birthDate.IsZero() {
				user.BornAt = &birthDate
				user.Details().BirthDay = birthDate.Day()
				user.Details().BirthMonth = int(birthDate.Month())
				user.Details().BirthYear = birthDate.Year()
			}

			// Update email, if verified.
			if userInfo.IsEmailVerified() {
				user.UserEmail = clean.Email(userInfo.GetEmail())
				user.VerifiedAt = entity.TimeStamp()
			}

			// Update user account.
			if err = user.Save(); err != nil {
				event.AuditErr([]string{clientIp, "oidc", actor, action, authn.ErrAccountUpdateFailed.Error(), err.Error()})
				c.HTML(http.StatusUnauthorized, "auth.gohtml", CreateSessionError(http.StatusUnauthorized, i18n.Error(i18n.ErrInvalidCredentials)))
				return
			}

			// Set user avatar image?
			if avatarUrl := userInfo.GetPicture(); avatarUrl == "" || user.HasAvatar() {
				// Do nothing.
			} else if err = avatar.SetUserImageURL(user, avatarUrl, entity.SrcOIDC); err != nil {
				event.AuditWarn([]string{clientIp, "oidc", actor, action, "failed to set avatar image", err.Error()})
			}
		} else if conf.OIDCRegister() {
			action = "sign up"

			// Create new user record.
			user = &oidcUser
			actor = user.Username()

			// Set user profile information.
			user.SetDisplayName(userInfo.GetName(), entity.SrcOIDC)
			user.SetGivenName(userInfo.GetGivenName())
			user.SetFamilyName(userInfo.GetFamilyName())
			user.Details().UserGender = clean.Name(string(userInfo.GetGender()))
			user.Details().NickName = clean.Name(userInfo.GetNickname())

			// Set user profile URL.
			user.Details().ProfileURL = clean.Uri(userInfo.GetProfile())

			// Set user site URL.
			user.Details().SiteURL = clean.Uri(userInfo.GetWebsite())

			// Set UI locale.
			user.Settings().UILanguage = clean.Locale(userInfo.GetLocale().String(), "")

			// Set UI timezone.
			user.Settings().UITimeZone = userInfo.GetZoneinfo()

			// Set user location, if available.
			if addr := userInfo.GetAddress(); addr != nil {
				user.Details().UserLocation = clean.Name(addr.GetLocality())
				user.Details().UserCountry = clean.TypeLowerUnderscore(addr.GetCountry())
			}

			// Set birthday, if available.
			if birthDate := txt.ParseTime(userInfo.GetBirthdate(), userInfo.GetZoneinfo()); !birthDate.IsZero() {
				user.BornAt = &birthDate
				user.Details().BirthDay = birthDate.Day()
				user.Details().BirthMonth = int(birthDate.Month())
				user.Details().BirthYear = birthDate.Year()
			}

			// Set email, if verified.
			if userInfo.IsEmailVerified() {
				user.UserEmail = clean.Email(userInfo.GetEmail())
				user.VerifiedAt = entity.TimeStamp()
			}

			// Set user role and permissions.
			user.SetRole(conf.OIDCRole().String())
			user.CanLogin = true
			user.WebDAV = conf.OIDCWebDAV()

			// Create user account.
			if err = user.Create(); err != nil {
				event.AuditErr([]string{clientIp, "oidc", actor, action, authn.ErrAccountCreateFailed.Error(), err.Error()})
				c.HTML(http.StatusUnauthorized, "auth.gohtml", CreateSessionError(http.StatusUnauthorized, i18n.Error(i18n.ErrInvalidCredentials)))
				return
			}

			// Set user avatar image.
			if avatarUrl := userInfo.GetPicture(); avatarUrl == "" {
				event.AuditDebug([]string{clientIp, "oidc", actor, action, "no avatar image provided"})
			} else if err = avatar.SetUserImageURL(user, avatarUrl, entity.SrcOIDC); err != nil {
				event.AuditWarn([]string{clientIp, "oidc", actor, action, "failed to set avatar image", err.Error()})
			}
		} else {
			event.AuditErr([]string{clientIp, "oidc", actor, action, authn.ErrRegistrationDisabled.Error()})
			c.HTML(http.StatusUnauthorized, "auth.gohtml", CreateSessionError(http.StatusUnauthorized, i18n.Error(i18n.ErrInvalidCredentials)))
			return
		}

		// Login allowed?
		if !user.CanLogIn() {
			event.AuditErr([]string{clientIp, "oidc", actor, action, authn.ErrAccountDisabled.Error()})
			c.HTML(http.StatusUnauthorized, "auth.gohtml", CreateSessionError(http.StatusUnauthorized, i18n.Error(i18n.ErrInvalidCredentials)))
			return
		}

		// Step 2: Create user session.
		sess := get.Session().New(c)
		sess.SetProvider(authn.ProviderOIDC)
		sess.SetMethod(authn.MethodDefault)
		sess.SetUser(user)
		sess.SetGrantType(authn.GrantAuthorizationCode)

		// Set identity provider tokens.
		sess.IdToken = tokens.IDToken
		sess.AccessToken = tokens.AccessToken
		sess.RefreshToken = tokens.RefreshToken

		// Set session expiration and timeout.
		sess.SetExpiresIn(unix.Day)
		sess.SetTimeout(-1)

		// Save session after successful authentication.
		if sess, err = get.Session().Save(sess); err != nil {
			event.AuditErr([]string{clientIp, "oidc", actor, action, "%s"}, err)
			c.HTML(http.StatusUnauthorized, "auth.gohtml", CreateSessionError(http.StatusUnauthorized, i18n.Error(i18n.ErrInvalidCredentials)))
			return
		} else if sess == nil {
			c.HTML(http.StatusUnauthorized, "auth.gohtml", CreateSessionError(http.StatusUnauthorized, i18n.Error(i18n.ErrUnexpected)))
			return
		}

		// Return the reserved request rate limit token after successful authentication.
		r.Success()

		// Response includes user data, session data, and client config values.
		response := CreateSessionResponse(sess.AuthToken(), sess, conf.ClientSession(sess))

		// Log success.
		event.AuditInfo([]string{clientIp, "oidc", actor, action, authn.Succeeded})

		// Update login timestamp.
		user.UpdateLoginTime()

		// Step 3: Render HTML template to set the access token in localStorage.
		c.HTML(http.StatusOK, "auth.gohtml", response)
	})
}