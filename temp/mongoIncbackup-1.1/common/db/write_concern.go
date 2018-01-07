package db

import (
	"fmt"
	"mongoIncbackup-1.1/common/json"
	"mongoIncbackup-1.1/common/log"
	"mongoIncbackup-1.1/common/util"
	"mongoIncbackup-1.1/mgo.v2"
	"strconv"
)

// write concern fields
const (
	j        = "j"
	w        = "w"
	fSync    = "fsync"
	wTimeout = "wtimeout"
)

// constructWCObject takes in a write concern and attempts to construct an
// mgo.Safe object from it. It returns an error if it is unable to parse the
// string or if a parsed write concern field value is invalid.
func constructWCObject(writeConcern string) (sessionSafety *mgo.Safe, err error) {
	sessionSafety = &mgo.Safe{}
	defer func() {
		// If the user passes a w value of 0, we set the session to use the
		// unacknowledged write concern but only if journal commit acknowledgment,
		// is not required. If commit acknowledgment is required, it prevails,
		// and the server will require that mongod acknowledge the write operation
		if sessionSafety.WMode == "" && sessionSafety.W == 0 && !sessionSafety.J {
			sessionSafety = nil
		}
	}()
	jsonWriteConcern := map[string]interface{}{}

	if err = json.Unmarshal([]byte(writeConcern), &jsonWriteConcern); err != nil {
		// if the writeConcern string can not be unmarshaled into JSON, this
		// allows a default to the old behavior wherein the entire argument
		// passed in is assigned to the 'w' field - thus allowing users pass
		// a write concern that looks like: "majority", 0, "4", etc.
		wValue, err := strconv.Atoi(writeConcern)
		if err != nil {
			sessionSafety.WMode = writeConcern
		} else {
			sessionSafety.W = wValue
			if wValue < 0 {
				return sessionSafety, fmt.Errorf("invalid '%v' argument: %v", w, wValue)
			}
		}
		return sessionSafety, nil
	}

	if jVal, ok := jsonWriteConcern[j]; ok && util.IsTruthy(jVal) {
		sessionSafety.J = true
	}

	if fsyncVal, ok := jsonWriteConcern[fSync]; ok && util.IsTruthy(fsyncVal) {
		sessionSafety.FSync = true
	}

	if wtimeout, ok := jsonWriteConcern[wTimeout]; ok {
		wtimeoutValue, err := util.ToInt(wtimeout)
		if err != nil {
			return sessionSafety, fmt.Errorf("invalid '%v' argument: %v", wTimeout, wtimeout)
		}
		sessionSafety.WTimeout = wtimeoutValue
	}

	if wInterface, ok := jsonWriteConcern[w]; ok {
		wValue, err := util.ToInt(wInterface)
		if err != nil {
			// if the argument is neither a string nor int, error out
			wStrVal, ok := wInterface.(string)
			if !ok {
				return sessionSafety, fmt.Errorf("invalid '%v' argument: %v", w, wInterface)
			}
			sessionSafety.WMode = wStrVal
		} else {
			sessionSafety.W = wValue
			if wValue < 0 {
				return sessionSafety, fmt.Errorf("invalid '%v' argument: %v", w, wValue)
			}
		}
	}
	return sessionSafety, nil
}

// BuildWriteConcern takes a string and a NodeType indicating the type of node the write concern
// is intended to be used against, and converts the write concern string argument into an
// mgo.Safe object that's usable on sessions for that node type.
func BuildWriteConcern(writeConcern string, nodeType NodeType) (*mgo.Safe, error) {
	sessionSafety, err := constructWCObject(writeConcern)
	if err != nil {
		return nil, err
	}

	if sessionSafety == nil {
		log.Logf(log.DebugLow, "using unacknowledged write concern")
		return nil, nil
	}

	// for standalone mongods, set the default write concern to 1
	if nodeType == Standalone {
		log.Logf(log.DebugLow, "standalone server: setting write concern %v to 1", w)
		sessionSafety.W = 1
		sessionSafety.WMode = ""
	}

	var writeConcernStr interface{}

	if sessionSafety.WMode != "" {
		writeConcernStr = sessionSafety.WMode
	} else {
		writeConcernStr = sessionSafety.W
	}
	log.Logf(log.Info, "using write concern: %v='%v', %v=%v, %v=%v, %v=%v",
		w, writeConcernStr,
		j, sessionSafety.J,
		fSync, sessionSafety.FSync,
		wTimeout, sessionSafety.WTimeout,
	)
	return sessionSafety, nil
}
