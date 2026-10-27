package store

import "time"

// Stars: photos an account likes, its own (user_id 0 is the one shared list
// of a server without accounts). Filter.Starred lists them.

// SetStar stars (on) or unstars a photo for user. Starring twice, or
// unstarring a photo not starred, changes nothing.
func (s *Store) SetStar(user, photo int64, on bool) error {
	if !on {
		_, err := s.DB.Exec(`DELETE FROM stars WHERE user_id = ? AND photo_id = ?`, user, photo)
		return err
	}
	res, err := s.DB.Exec(`INSERT OR IGNORE INTO stars (user_id, photo_id, at) SELECT ?, id, ? FROM photos WHERE id = ?`,
		user, time.Now().Unix(), photo)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var ok bool
		if err := s.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM photos WHERE id = ?)`, photo).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return ErrNoPhoto
		}
	}
	return nil
}

// Starred returns which of ids user has starred.
func (s *Store) Starred(user int64, ids []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(len(ids), start+500)]
		rows, err := s.DB.Query(`SELECT photo_id FROM stars WHERE user_id = ? AND photo_id IN (`+placeholders(len(chunk))+`)`,
			append([]any{user}, int64sToAny(chunk)...)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// StarCount is how many photos user has starred.
func (s *Store) StarCount(user int64) (int, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM stars WHERE user_id = ?`, user).Scan(&n)
	return n, err
}
