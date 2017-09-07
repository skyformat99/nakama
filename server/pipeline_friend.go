// Copyright 2017 The Nakama Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"database/sql"
	"errors"

	"encoding/json"
	"fmt"
	"github.com/lib/pq"
	"github.com/satori/go.uuid"
	"go.uber.org/zap"
)

func (p *pipeline) querySocialGraph(logger *zap.Logger, filterQuery string, params []interface{}) ([]*User, error) {
	users := []*User{}

	query := `
SELECT id, handle, fullname, avatar_url,
	lang, location, timezone, metadata,
	created_at, users.updated_at, last_online_at
FROM users ` + filterQuery

	rows, err := p.db.Query(query, params...)
	if err != nil {
		logger.Error("Could not execute social graph query", zap.String("query", query), zap.Error(err))
		return nil, err
	}
	defer rows.Close()

	var id []byte
	var handle sql.NullString
	var fullname sql.NullString
	var avatarURL sql.NullString
	var lang sql.NullString
	var location sql.NullString
	var timezone sql.NullString
	var metadata []byte
	var createdAt sql.NullInt64
	var updatedAt sql.NullInt64
	var lastOnlineAt sql.NullInt64

	for rows.Next() {
		err = rows.Scan(&id, &handle, &fullname, &avatarURL, &lang, &location, &timezone, &metadata, &createdAt, &updatedAt, &lastOnlineAt)
		if err != nil {
			logger.Error("Could not execute social graph query", zap.Error(err))
			return nil, err
		}

		users = append(users, &User{
			Id:           id,
			Handle:       handle.String,
			Fullname:     fullname.String,
			AvatarUrl:    avatarURL.String,
			Lang:         lang.String,
			Location:     location.String,
			Timezone:     timezone.String,
			Metadata:     metadata,
			CreatedAt:    createdAt.Int64,
			UpdatedAt:    updatedAt.Int64,
			LastOnlineAt: lastOnlineAt.Int64,
		})
	}
	if err = rows.Err(); err != nil {
		logger.Error("Could not execute social graph query", zap.Error(err))
		return nil, err
	}

	return users, nil
}

func (p *pipeline) addFacebookFriends(logger *zap.Logger, userID []byte, handle string, fbid string, accessToken string) {
	var tx *sql.Tx
	var err error

	ts := nowMs()
	friendUserIDs := make([]interface{}, 0)
	defer func() {
		if err != nil {
			logger.Error("Could not import friends from Facebook", zap.Error(err))
			if tx != nil {
				err = tx.Rollback()
				if err != nil {
					logger.Error("Could not rollback transaction", zap.Error(err))
				}
			}
		} else {
			if tx != nil {
				err = tx.Commit()
				if err != nil {
					logger.Error("Could not commit transaction", zap.Error(err))
				} else {
					logger.Debug("Imported friends from Facebook")

					// Send out notifications.
					if len(friendUserIDs) != 0 {
						content, err := json.Marshal(map[string]interface{}{"handle": handle, "facebook_id": fbid})
						if err != nil {
							logger.Warn("Failed to send Facebook friend join notifications", zap.Error(err))
							return
						}
						subject := "Your friend has just joined the game"
						expiresAt := ts + p.notificationService.expiryMs

						notifications := make([]*NNotification, len(friendUserIDs))
						for i, friendUserID := range friendUserIDs {
							fid := friendUserID.([]byte)
							notifications[i] = &NNotification{
								Id:         uuid.NewV4().Bytes(),
								UserID:     fid,
								Subject:    subject,
								Content:    content,
								Code:       NOTIFICATION_FRIEND_JOIN_GAME,
								SenderID:   userID,
								CreatedAt:  ts,
								ExpiresAt:  expiresAt,
								Persistent: true,
							}
						}

						err = p.notificationService.NotificationSend(notifications)
						if err != nil {
							logger.Warn("Failed to send Facebook friend join notifications", zap.Error(err))
						}
					}
				}
			}
		}
	}()

	fbFriends, err := p.socialClient.GetFacebookFriends(accessToken)
	if err != nil {
		return
	}
	if len(fbFriends) == 0 {
		return
	}

	tx, err = p.db.Begin()
	if err != nil {
		return
	}

	query := "SELECT id FROM users WHERE facebook_id IN ("
	friends := make([]interface{}, len(fbFriends))
	for i, fbFriend := range fbFriends {
		if i != 0 {
			query += ", "
		}
		query += fmt.Sprintf("$%v", i+1)
		friends[i] = fbFriend.ID
	}
	query += ")"
	rows, err := tx.Query(query, friends...)
	if err != nil {
		return
	}
	defer rows.Close()

	queryEdge := "INSERT INTO user_edge (source_id, position, updated_at, destination_id, state) VALUES "
	paramsEdge := []interface{}{userID, ts}
	queryEdgeMetadata := "UPDATE user_edge_metadata SET count = count + 1, updated_at = $1 WHERE source_id IN ("
	paramsEdgeMetadata := []interface{}{ts}
	for rows.Next() {
		var currentUser []byte
		err = rows.Scan(&currentUser)
		if err != nil {
			return
		}

		if len(paramsEdge) != 2 {
			queryEdge += ", "
		}
		paramsEdge = append(paramsEdge, currentUser)
		queryEdge += fmt.Sprintf("($1, $2, $2, $%v, 0), ($%v, $2, $2, $1, 0)", len(paramsEdge), len(paramsEdge))

		if len(paramsEdgeMetadata) != 1 {
			queryEdgeMetadata += ", "
		}
		paramsEdgeMetadata = append(paramsEdgeMetadata, currentUser)
		queryEdgeMetadata += fmt.Sprintf("$%v", len(paramsEdgeMetadata))
	}
	err = rows.Err()
	if err != nil {
		return
	}
	queryEdgeMetadata += ")"

	// Check if any Facebook friends are already users, if not there are no new edges to handle.
	if len(paramsEdge) <= 2 {
		return
	}

	// Insert new friend relationship edges.
	_, err = tx.Exec(queryEdge, paramsEdge...)
	if err != nil {
		return
	}
	// Update edge metadata for each user to increment count.
	_, err = tx.Exec(queryEdgeMetadata, paramsEdgeMetadata...)
	if err != nil {
		return
	}
	// Update edge metadata for current user to bump count by number of new friends.
	_, err = tx.Exec(`UPDATE user_edge_metadata SET count = $1, updated_at = $2 WHERE source_id = $3`, len(paramsEdge)-2, ts, userID)
	if err != nil {
		return
	}

	// Track the user IDs to notify their friend has joined the game.
	friendUserIDs = paramsEdge[2:]
}

func (p *pipeline) getFriends(filterQuery string, userID []byte) ([]*Friend, error) {
	query := `
SELECT id, handle, fullname, avatar_url,
	lang, location, timezone, metadata,
	created_at, users.updated_at, last_online_at, state
FROM users, user_edge ` + filterQuery

	rows, err := p.db.Query(query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	friends := make([]*Friend, 0)

	for rows.Next() {
		var id []byte
		var handle sql.NullString
		var fullname sql.NullString
		var avatarURL sql.NullString
		var lang sql.NullString
		var location sql.NullString
		var timezone sql.NullString
		var metadata []byte
		var createdAt sql.NullInt64
		var updatedAt sql.NullInt64
		var lastOnlineAt sql.NullInt64
		var state sql.NullInt64

		err = rows.Scan(&id, &handle, &fullname, &avatarURL, &lang, &location, &timezone, &metadata, &createdAt, &updatedAt, &lastOnlineAt, &state)
		if err != nil {
			return nil, err
		}

		friends = append(friends, &Friend{
			User: &User{
				Id:           id,
				Handle:       handle.String,
				Fullname:     fullname.String,
				AvatarUrl:    avatarURL.String,
				Lang:         lang.String,
				Location:     location.String,
				Timezone:     timezone.String,
				Metadata:     metadata,
				CreatedAt:    createdAt.Int64,
				UpdatedAt:    updatedAt.Int64,
				LastOnlineAt: lastOnlineAt.Int64,
			},
			State: state.Int64,
		})
	}

	return friends, nil
}

func (p *pipeline) friendAdd(l *zap.Logger, session session, envelope *Envelope) {
	e := envelope.GetFriendsAdd()

	if len(e.Friends) == 0 {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "At least one friend must be present"))
		return
	} else if len(e.Friends) > 1 {
		l.Warn("There are more than one friend passed to the request - only processing the first item of the list.")
	}

	f := e.Friends[0]
	switch f.Id.(type) {
	case *TFriendsAdd_FriendsAdd_UserId:
		p.friendAddById(l, session, envelope, f.GetUserId())
	case *TFriendsAdd_FriendsAdd_Handle:
		p.friendAddByHandle(l, session, envelope, f.GetHandle())
	}
}

func (p *pipeline) friendAddById(l *zap.Logger, session session, envelope *Envelope, friendIdBytes []byte) {
	if len(friendIdBytes) == 0 {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "User ID must be present"))
		return
	}
	friendID, err := uuid.FromBytes(friendIdBytes)
	if err != nil {
		l.Warn("Could not add friend", zap.Error(err))
		session.Send(ErrorMessageBadInput(envelope.CollationId, "Invalid User ID"))
		return
	}

	logger := l.With(zap.String("friend_id", friendID.String()))
	if friendID == session.UserID() {
		logger.Warn("Cannot add self", zap.Error(err))
		session.Send(ErrorMessageBadInput(envelope.CollationId, "Cannot add self"))
		return
	}

	if err := friendAdd(logger, p.db, p.notificationService, session.UserID().Bytes(), session.Handle(), friendID.Bytes()); err != nil {
		logger.Error("Could not add friend", zap.Error(err))
		session.Send(ErrorMessageRuntimeException(envelope.CollationId, "Failed to add friend"))
		return
	}

	logger.Debug("Added friend")
	session.Send(&Envelope{CollationId: envelope.CollationId})
}

func (p *pipeline) friendAddByHandle(l *zap.Logger, session session, envelope *Envelope, friendHandle string) {
	if friendHandle == "" || friendHandle == session.Handle() {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "User handle must be present and not equal to user's handle"))
		return
	}

	logger := l.With(zap.String("friend_handle", friendHandle))
	if err := friendAddHandle(logger, p.db, p.notificationService, session.UserID().Bytes(), session.Handle(), friendHandle); err != nil {
		logger.Error("Could not add friend", zap.Error(err))
		session.Send(ErrorMessageRuntimeException(envelope.CollationId, "Failed to add friend"))
		return
	}

	logger.Debug("Added friend")
	session.Send(&Envelope{CollationId: envelope.CollationId})
}

func (p *pipeline) friendRemove(l *zap.Logger, session session, envelope *Envelope) {
	e := envelope.GetFriendsRemove()

	if len(e.UserIds) == 0 {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "At least one user ID must be present"))
		return
	} else if len(e.UserIds) > 1 {
		l.Warn("There are more than one user ID passed to the request - only processing the first item of the list.")
	}

	removeFriendRequest := e.UserIds[0]
	if len(removeFriendRequest) == 0 {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "User ID must be present"))
		return
	}

	friendID, err := uuid.FromBytes(removeFriendRequest)
	if err != nil {
		l.Warn("Could not add friend", zap.Error(err))
		session.Send(ErrorMessageBadInput(envelope.CollationId, "Invalid User ID"))
		return
	}
	logger := l.With(zap.String("friend_id", friendID.String()))
	friendIDBytes := friendID.Bytes()

	if friendID == session.UserID() {
		logger.Warn("Cannot remove self", zap.Error(err))
		session.Send(ErrorMessageBadInput(envelope.CollationId, "Cannot remove self"))
		return
	}

	tx, err := p.db.Begin()
	if err != nil {
		logger.Error("Could not remove friend", zap.Error(err))
		session.Send(ErrorMessageRuntimeException(envelope.CollationId, "Failed to remove friend"))
		return
	}
	defer func() {
		if err != nil {
			logger.Error("Could not remove friend", zap.Error(err))
			err = tx.Rollback()
			if err != nil {
				logger.Error("Could not rollback transaction", zap.Error(err))
			}

			session.Send(ErrorMessageRuntimeException(envelope.CollationId, "Failed to remove friend"))
		} else {
			err = tx.Commit()
			if err != nil {
				logger.Error("Could not commit transaction", zap.Error(err))
				session.Send(ErrorMessageRuntimeException(envelope.CollationId, "Failed to remove friend"))
			} else {
				logger.Info("Removed friend")
				session.Send(&Envelope{CollationId: envelope.CollationId})
			}
		}
	}()

	updatedAt := nowMs()

	res, err := tx.Exec("DELETE FROM user_edge WHERE source_id = $1 AND destination_id = $2", session.UserID().Bytes(), friendIDBytes)
	rowsAffected, _ := res.RowsAffected()
	if err == nil && rowsAffected > 0 {
		_, err = tx.Exec("UPDATE user_edge_metadata SET count = count - 1, updated_at = $2 WHERE source_id = $1", session.UserID().Bytes(), updatedAt)
	}

	if err != nil {
		return
	}

	res, err = tx.Exec("DELETE FROM user_edge WHERE source_id = $1 AND destination_id = $2", friendIDBytes, session.UserID().Bytes())
	rowsAffected, _ = res.RowsAffected()
	if err == nil && rowsAffected > 0 {
		_, err = tx.Exec("UPDATE user_edge_metadata SET count = count - 1, updated_at = $2 WHERE source_id = $1", friendIDBytes, updatedAt)
	}
}

func (p *pipeline) friendBlock(l *zap.Logger, session session, envelope *Envelope) {
	e := envelope.GetFriendsBlock()

	if len(e.UserIds) == 0 {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "At least one user ID must be present"))
		return
	} else if len(e.UserIds) > 1 {
		l.Warn("There are more than one user ID passed to the request - only processing the first item of the list.")
	}

	blockUserRequest := e.UserIds[0]
	if len(blockUserRequest) == 0 {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "User ID must be present"))
		return
	}

	userID, err := uuid.FromBytes(blockUserRequest)
	if err != nil {
		l.Warn("Could not block user", zap.Error(err))
		session.Send(ErrorMessageBadInput(envelope.CollationId, "Invalid User ID"))
		return
	}
	logger := l.With(zap.String("user_id", userID.String()))
	userIDBytes := userID.Bytes()

	if userID == session.UserID() {
		logger.Warn("Cannot block self", zap.Error(err))
		session.Send(ErrorMessageBadInput(envelope.CollationId, "Cannot block self"))
		return
	}

	tx, err := p.db.Begin()
	if err != nil {
		logger.Error("Could not block user", zap.Error(err))
		session.Send(ErrorMessageRuntimeException(envelope.CollationId, "Failed to block friend"))
		return
	}
	defer func() {
		if err != nil {
			if _, ok := err.(*pq.Error); ok {
				logger.Error("Could not block user", zap.Error(err))
			} else {
				logger.Warn("Could not block user", zap.Error(err))
			}
			err = tx.Rollback()
			if err != nil {
				logger.Error("Could not rollback transaction", zap.Error(err))
			}

			session.Send(ErrorMessageRuntimeException(envelope.CollationId, "Could not block user"))
		} else {
			err = tx.Commit()
			if err != nil {
				logger.Error("Could not commit transaction", zap.Error(err))
				session.Send(ErrorMessageRuntimeException(envelope.CollationId, "Could not block user"))
			} else {
				logger.Info("User blocked")
				session.Send(&Envelope{CollationId: envelope.CollationId})
			}
		}
	}()

	res, err := tx.Exec("UPDATE user_edge SET state = 3, updated_at = $3 WHERE source_id = $1 AND destination_id = $2",
		session.UserID().Bytes(), userIDBytes, nowMs())

	if err != nil {
		return
	}

	if rowsAffected, _ := res.RowsAffected(); rowsAffected == 0 {
		err = errors.New("Could not block user. User ID may not exist")
		return
	}

	// Delete opposite relationship if user hasn't blocked you already
	res, err = tx.Exec("DELETE FROM user_edge WHERE source_id = $1 AND destination_id = $2 AND state != 3",
		userIDBytes, session.UserID().Bytes())

	if err != nil {
		return
	}

	if rowsAffected, _ := res.RowsAffected(); rowsAffected == 1 {
		_, err = tx.Exec("UPDATE user_edge_metadata SET count = count - 1, updated_at = $2 WHERE source_id = $1", userIDBytes, nowMs())
	}
}

func (p *pipeline) friendsList(logger *zap.Logger, session session, envelope *Envelope) {
	friends, err := p.getFriends("WHERE id = destination_id AND source_id = $1", session.UserID().Bytes())
	if err != nil {
		logger.Error("Could not get friends", zap.Error(err))
		session.Send(ErrorMessageRuntimeException(envelope.CollationId, "Could not get friends"))
		return
	}

	session.Send(&Envelope{CollationId: envelope.CollationId, Payload: &Envelope_Friends{Friends: &TFriends{Friends: friends}}})
}
