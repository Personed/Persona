package client

import "encoding/json"

// Work is the work.get output and each work.subscribe event.
type Work struct {
	Memory              []Memory `json:"memory"`
	MemoryReviewEnabled bool     `json:"memoryReviewEnabled"`
	Skills              []Skill  `json:"skills"`
	Routines            struct {
		Items []Routine    `json:"items"`
		Runs  []RoutineRun `json:"runs"`
	} `json:"routines"`
}

// Memory is one memory record.
type Memory struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	Category string `json:"category,omitempty"`
	Revision int    `json:"revision"`
}

// Skill is a managed skill; EnabledBots lists the Bots that may use it.
type Skill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	EnabledBots []int  `json:"enabledBots"`
}

// Schedule is a routine schedule: once at a time, or a cron expression.
type Schedule struct {
	Type       string `json:"type"`
	At         string `json:"at,omitempty"`
	Expression string `json:"expression,omitempty"`
	TimeZone   string `json:"timeZone"`
}

// Routine is a scheduled task of a Bot. NextAt is epoch milliseconds.
type Routine struct {
	ID           string          `json:"id"`
	Title        string          `json:"title"`
	Instructions string          `json:"instructions"`
	Schedule     Schedule        `json:"schedule"`
	SkillIDs     []string        `json:"skillIds"`
	Enabled      bool            `json:"enabled"`
	NextAt       json.RawMessage `json:"nextAt"`
	Revision     int             `json:"revision"`
}

// RoutineRun is one scheduled or test run of a routine. Times are epoch milliseconds.
type RoutineRun struct {
	ID         string          `json:"id"`
	RoutineID  string          `json:"routineId"`
	Title      string          `json:"title"`
	Mode       string          `json:"mode"`
	FiredAt    json.RawMessage `json:"firedAt"`
	FinishedAt json.RawMessage `json:"finishedAt"`
	Status     string          `json:"status"`
	Error      string          `json:"error"`
}
