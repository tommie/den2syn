package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/sirupsen/logrus"
)

// UDFManager handles User-Defined Function implementations
type UDFManager struct {
	dendriteDB *sql.DB
	synapseDB  *sql.DB
	verbose    bool
	log        *logrus.Logger
}

// NewUDFManager creates a new UDF manager
func NewUDFManager(dendriteDB, synapseDB *sql.DB, verbose bool, log *logrus.Logger) *UDFManager {
	return &UDFManager{
		dendriteDB: dendriteDB,
		synapseDB:  synapseDB,
		verbose:    verbose,
		log:        log,
	}
}

// EventMetadata represents extracted event metadata
type EventMetadata struct {
	Sender         string                 `json:"sender"`
	Type           string                 `json:"type"`
	StateKey       *string                `json:"state_key,omitempty"`
	Content        map[string]interface{} `json:"content"`
	OriginServerTS int64                  `json:"origin_server_ts"`
	Membership     string                 `json:"membership,omitempty"`
}

// ExtractEventMetadata extracts structured metadata from Dendrite event JSON
func (u *UDFManager) ExtractEventMetadata(eventJSON string) (*EventMetadata, error) {
	var rawEvent map[string]interface{}
	if err := json.Unmarshal([]byte(eventJSON), &rawEvent); err != nil {
		return nil, fmt.Errorf("failed to parse event JSON: %w", err)
	}

	metadata := &EventMetadata{
		Content: make(map[string]interface{}),
	}

	// Extract sender
	if sender, ok := rawEvent["sender"].(string); ok {
		metadata.Sender = sender
	}

	// Extract type
	if eventType, ok := rawEvent["type"].(string); ok {
		metadata.Type = eventType
	}

	// Extract state key
	if stateKey, exists := rawEvent["state_key"]; exists {
		if stateKeyStr, ok := stateKey.(string); ok {
			metadata.StateKey = &stateKeyStr
		}
	}

	// Extract content
	if content, ok := rawEvent["content"].(map[string]interface{}); ok {
		metadata.Content = content

		// Extract membership if this is a membership event
		if membership, ok := content["membership"].(string); ok {
			metadata.Membership = membership
		}
	}

	// Extract origin_server_ts
	if originServerTS, ok := rawEvent["origin_server_ts"].(float64); ok {
		metadata.OriginServerTS = int64(originServerTS)
	}

	return metadata, nil
}

// CalculateTopologicalOrdering calculates topological ordering for events
func (u *UDFManager) CalculateTopologicalOrdering(tx *sql.Tx, eventID, roomID string) (int64, error) {
	// This is a simplified implementation
	// In a real implementation, this would need to:
	// 1. Build the full event DAG for the room
	// 2. Perform topological sort
	// 3. Assign ordering values that respect causality

	query := `
		SELECT depth 
		FROM roomserver_events e
		JOIN nid_room_mapping rm ON e.room_nid = rm.room_nid
		WHERE e.event_id = $1 AND rm.room_id = $2
	`

	var depth int64
	err := tx.QueryRow(query, eventID, roomID).Scan(&depth)
	if err != nil {
		return 0, fmt.Errorf("failed to get event depth: %w", err)
	}

	// For now, use depth as a proxy for topological ordering
	// This is not correct for all cases but provides a starting point
	return depth, nil
}

// BuildStateGroup creates or finds appropriate state group for room state
func (u *UDFManager) BuildStateGroup(tx *sql.Tx, roomID, eventID string) (int64, error) {
	// This is a simplified implementation
	// Real implementation would need to:
	// 1. Calculate the state at this event
	// 2. Check if an existing state group has the same state
	// 3. Create new state group if needed
	// 4. Handle state group edges for efficient storage

	// For now, create a simple mapping based on room and some hash
	query := `
		SELECT COUNT(*) + 1 
		FROM state_groups 
		WHERE room_id = $1
	`

	var stateGroupID int64
	err := tx.QueryRow(query, roomID).Scan(&stateGroupID)
	if err != nil {
		return 0, fmt.Errorf("failed to calculate state group ID: %w", err)
	}

	return stateGroupID, nil
}

// ConvertAuthEvents converts Dendrite auth event NIDs to Synapse event IDs
func (u *UDFManager) ConvertAuthEvents(tx *sql.Tx, authEventNIDs []int64) ([]string, error) {
	if len(authEventNIDs) == 0 {
		return []string{}, nil
	}

	// Build placeholders for the query
	placeholders := make([]string, len(authEventNIDs))
	args := make([]interface{}, len(authEventNIDs))
	for i, nid := range authEventNIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = nid
	}

	query := fmt.Sprintf(`
		SELECT event_id 
		FROM nid_event_mapping 
		WHERE event_nid IN (%s)
		ORDER BY event_nid
	`, strings.Join(placeholders, ","))

	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query auth events: %w", err)
	}
	defer rows.Close()

	var eventIDs []string
	for rows.Next() {
		var eventID string
		if err := rows.Scan(&eventID); err != nil {
			return nil, fmt.Errorf("failed to scan event ID: %w", err)
		}
		eventIDs = append(eventIDs, eventID)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating auth events: %w", err)
	}

	return eventIDs, nil
}

// NIDResolver provides methods for resolving Dendrite NIDs to strings
type NIDResolver struct {
	tx *sql.Tx
}

// NewNIDResolver creates a new NID resolver
func NewNIDResolver(tx *sql.Tx) *NIDResolver {
	return &NIDResolver{tx: tx}
}

// ResolveRoomNID converts room NID to room ID
func (r *NIDResolver) ResolveRoomNID(roomNID int64) (string, error) {
	var roomID string
	err := r.tx.QueryRow("SELECT room_id FROM nid_room_mapping WHERE room_nid = $1", roomNID).Scan(&roomID)
	if err != nil {
		return "", fmt.Errorf("failed to resolve room NID %d: %w", roomNID, err)
	}
	return roomID, nil
}

// ResolveEventNID converts event NID to event ID
func (r *NIDResolver) ResolveEventNID(eventNID int64) (string, error) {
	var eventID string
	err := r.tx.QueryRow("SELECT event_id FROM nid_event_mapping WHERE event_nid = $1", eventNID).Scan(&eventID)
	if err != nil {
		return "", fmt.Errorf("failed to resolve event NID %d: %w", eventNID, err)
	}
	return eventID, nil
}

// ResolveEventTypeNID converts event type NID to event type string
func (r *NIDResolver) ResolveEventTypeNID(eventTypeNID int64) (string, error) {
	var eventType string
	err := r.tx.QueryRow("SELECT event_type FROM nid_eventtype_mapping WHERE event_type_nid = $1", eventTypeNID).Scan(&eventType)
	if err != nil {
		return "", fmt.Errorf("failed to resolve event type NID %d: %w", eventTypeNID, err)
	}
	return eventType, nil
}

// ResolveStateKeyNID converts state key NID to state key string
func (r *NIDResolver) ResolveStateKeyNID(stateKeyNID int64) (string, error) {
	var stateKey string
	err := r.tx.QueryRow("SELECT event_state_key FROM nid_statekey_mapping WHERE event_state_key_nid = $1", stateKeyNID).Scan(&stateKey)
	if err != nil {
		return "", fmt.Errorf("failed to resolve state key NID %d: %w", stateKeyNID, err)
	}
	return stateKey, nil
}

// EventDAG represents the directed acyclic graph of events in a room
type EventDAG struct {
	events map[string]*DAGEvent
	roots  []string
	roomID string
}

// DAGEvent represents an event in the DAG
type DAGEvent struct {
	EventID    string
	Depth      int64
	PrevEvents []string
	AuthEvents []string
	Children   []string
}

// BuildEventDAG constructs the event DAG for topological ordering
func (u *UDFManager) BuildEventDAG(tx *sql.Tx, roomID string) (*EventDAG, error) {
	dag := &EventDAG{
		events: make(map[string]*DAGEvent),
		roomID: roomID,
	}

	// Query all events in the room with their relationships
	query := `
		SELECT 
			e.event_id,
			e.depth,
			COALESCE(array_agg(DISTINCT pe.event_id) FILTER (WHERE pe.event_id IS NOT NULL), '{}') as prev_events,
			COALESCE(array_agg(DISTINCT ae.event_id) FILTER (WHERE ae.event_id IS NOT NULL), '{}') as auth_events
		FROM roomserver_events e
		JOIN nid_room_mapping rm ON e.room_nid = rm.room_nid
		LEFT JOIN roomserver_previous_events rpe ON e.event_nid = rpe.event_nid
		LEFT JOIN nid_event_mapping pe ON rpe.previous_event_nid = pe.event_nid
		LEFT JOIN nid_event_mapping ae ON ae.event_nid = ANY(e.auth_event_nids)
		WHERE rm.room_id = $1
		GROUP BY e.event_id, e.depth
		ORDER BY e.depth
	`

	rows, err := tx.Query(query, roomID)
	if err != nil {
		return nil, fmt.Errorf("failed to query room events: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var eventID string
		var depth int64
		var prevEvents, authEvents []string

		err := rows.Scan(&eventID, &depth, &prevEvents, &authEvents)
		if err != nil {
			return nil, fmt.Errorf("failed to scan event: %w", err)
		}

		event := &DAGEvent{
			EventID:    eventID,
			Depth:      depth,
			PrevEvents: prevEvents,
			AuthEvents: authEvents,
			Children:   make([]string, 0),
		}

		dag.events[eventID] = event

		// Track roots (events with no previous events)
		if len(prevEvents) == 0 {
			dag.roots = append(dag.roots, eventID)
		}

		// Build children relationships
		for _, prevEventID := range prevEvents {
			if prevEvent, exists := dag.events[prevEventID]; exists {
				prevEvent.Children = append(prevEvent.Children, eventID)
			}
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating events: %w", err)
	}

	return dag, nil
}

// ComputeTopologicalOrdering computes topological ordering for all events in the DAG
func (dag *EventDAG) ComputeTopologicalOrdering() map[string]int64 {
	ordering := make(map[string]int64)
	visited := make(map[string]bool)
	order := int64(1)

	// Depth-first traversal starting from roots
	var visit func(string)
	visit = func(eventID string) {
		if visited[eventID] {
			return
		}

		visited[eventID] = true

		// Visit all dependencies first (prev events and auth events)
		event := dag.events[eventID]
		for _, prevEventID := range event.PrevEvents {
			if _, exists := dag.events[prevEventID]; exists {
				visit(prevEventID)
			}
		}

		for _, authEventID := range event.AuthEvents {
			if _, exists := dag.events[authEventID]; exists {
				visit(authEventID)
			}
		}

		// Assign ordering after dependencies
		ordering[eventID] = order
		order++
	}

	// Start from root events and traverse
	sortedRoots := make([]string, len(dag.roots))
	copy(sortedRoots, dag.roots)
	sort.Strings(sortedRoots) // Deterministic ordering

	for _, rootEventID := range sortedRoots {
		visit(rootEventID)
	}

	// Handle any remaining unvisited events (shouldn't happen in a valid DAG)
	for eventID := range dag.events {
		if !visited[eventID] {
			visit(eventID)
		}
	}

	return ordering
}
