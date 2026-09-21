package policy

import (
	"encoding/json"
	"fmt"
	"time"
)

// OnConditionFalse controls what happens to a policy's running script
// instances when its condition transitions from true to false.
type OnConditionFalse int

const (
	// Interrupt cancels the script at the next VM instruction boundary.
	Interrupt OnConditionFalse = iota
	// Complete allows the script to run to completion.
	Complete
)

// Policy associates a boolean condition expression with a Lua script: the
// script runs on every false-to-true transition of the expression, and
// OnConditionFalse governs what happens to any run still in flight when the
// expression goes back to false.
type Policy struct {
	ID               string
	ConditionExpr    ConditionExpr
	Script           string
	OnConditionFalse OnConditionFalse
}

// ExecutionStatus is the outcome of a single script execution.
type ExecutionStatus string

const (
	StatusRunning ExecutionStatus = "running"
	StatusSuccess ExecutionStatus = "success"
	// StatusFailure is a script-level failure (the script itself signalled
	// an error, e.g. via Lua's error()).
	StatusFailure ExecutionStatus = "failure"
	// StatusError is a VM or runtime error (e.g. a binding call failed, or
	// the execution was interrupted).
	StatusError ExecutionStatus = "error"
)

// ExecutionLog is one record of a single script execution.
type ExecutionLog struct {
	PolicyID  string
	StartedAt time.Time
	EndedAt   time.Time
	Status    ExecutionStatus
	Error     string
}

// ConditionExpr is a Boolean expression over named condition type
// instantiations. build resolves it against a ConditionRegistry into a live
// Condition tree.
type ConditionExpr interface {
	build(r *ConditionRegistry) (Condition, error)
	// conditionTypeNames adds every condition type name this expression (or
	// any sub-expression) references into out. It's how Engine tracks which
	// registered policies are affected when a condition type is
	// unregistered.
	conditionTypeNames(out map[string]struct{})
}

type useExpr struct {
	name   string
	params any
}

// Use references a registered condition type by name, instantiated with
// params.
func Use[P any](name string, params P) ConditionExpr {
	return &useExpr{name: name, params: params}
}

func (u *useExpr) build(r *ConditionRegistry) (Condition, error) {
	return r.build(u.name, u.params)
}

func (u *useExpr) conditionTypeNames(out map[string]struct{}) {
	out[u.name] = struct{}{}
}

type andExpr struct{ exprs []ConditionExpr }

// ExprAnd builds a Condition that is true only while every sub-expression
// is true.
func ExprAnd(exprs ...ConditionExpr) ConditionExpr {
	return &andExpr{exprs: exprs}
}

func (e *andExpr) build(r *ConditionRegistry) (Condition, error) {
	children, err := buildAll(r, e.exprs)
	if err != nil {
		return nil, err
	}
	return And(children...), nil
}

func (e *andExpr) conditionTypeNames(out map[string]struct{}) {
	for _, expr := range e.exprs {
		expr.conditionTypeNames(out)
	}
}

type orExpr struct{ exprs []ConditionExpr }

// ExprOr builds a Condition that is true while any sub-expression is true.
func ExprOr(exprs ...ConditionExpr) ConditionExpr {
	return &orExpr{exprs: exprs}
}

func (e *orExpr) build(r *ConditionRegistry) (Condition, error) {
	children, err := buildAll(r, e.exprs)
	if err != nil {
		return nil, err
	}
	return Or(children...), nil
}

func (e *orExpr) conditionTypeNames(out map[string]struct{}) {
	for _, expr := range e.exprs {
		expr.conditionTypeNames(out)
	}
}

type notExpr struct{ expr ConditionExpr }

// ExprNot builds the logical negation of expr.
func ExprNot(expr ConditionExpr) ConditionExpr {
	return &notExpr{expr: expr}
}

func (e *notExpr) build(r *ConditionRegistry) (Condition, error) {
	child, err := e.expr.build(r)
	if err != nil {
		return nil, err
	}
	return Not(child), nil
}

func (e *notExpr) conditionTypeNames(out map[string]struct{}) {
	e.expr.conditionTypeNames(out)
}

func buildAll(r *ConditionRegistry, exprs []ConditionExpr) ([]Condition, error) {
	conds := make([]Condition, 0, len(exprs))
	for _, expr := range exprs {
		c, err := expr.build(r)
		if err != nil {
			return nil, err
		}
		conds = append(conds, c)
	}
	return conds, nil
}

// conditionExprJSON is the wire format a ConditionExpr tree is persisted as.
// Type is a discriminator: "use" nodes carry Name/Params, "and"/"or" carry
// Children, and "not" carries a single-element Children.
type conditionExprJSON struct {
	Type     string              `json:"type"`
	Name     string              `json:"name,omitempty"`
	Params   json.RawMessage     `json:"params,omitempty"`
	Children []conditionExprJSON `json:"children,omitempty"`
}

// MarshalConditionExpr serialises expr to JSON for persistence. See
// UnmarshalConditionExpr for the reverse.
func MarshalConditionExpr(expr ConditionExpr) ([]byte, error) {
	node, err := marshalConditionExprNode(expr)
	if err != nil {
		return nil, err
	}
	return json.Marshal(node)
}

func marshalConditionExprNode(expr ConditionExpr) (conditionExprJSON, error) {
	switch e := expr.(type) {
	case *useExpr:
		params, err := json.Marshal(e.params)
		if err != nil {
			return conditionExprJSON{}, fmt.Errorf("policy: marshalling params for condition type %q: %w", e.name, err)
		}
		return conditionExprJSON{Type: "use", Name: e.name, Params: params}, nil
	case *andExpr:
		children, err := marshalConditionExprNodes(e.exprs)
		if err != nil {
			return conditionExprJSON{}, err
		}
		return conditionExprJSON{Type: "and", Children: children}, nil
	case *orExpr:
		children, err := marshalConditionExprNodes(e.exprs)
		if err != nil {
			return conditionExprJSON{}, err
		}
		return conditionExprJSON{Type: "or", Children: children}, nil
	case *notExpr:
		child, err := marshalConditionExprNode(e.expr)
		if err != nil {
			return conditionExprJSON{}, err
		}
		return conditionExprJSON{Type: "not", Children: []conditionExprJSON{child}}, nil
	default:
		return conditionExprJSON{}, fmt.Errorf("policy: cannot marshal condition expression of type %T", expr)
	}
}

func marshalConditionExprNodes(exprs []ConditionExpr) ([]conditionExprJSON, error) {
	nodes := make([]conditionExprJSON, 0, len(exprs))
	for _, expr := range exprs {
		node, err := marshalConditionExprNode(expr)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, nil
}

// UnmarshalConditionExpr deserialises data (as produced by
// MarshalConditionExpr) back into a ConditionExpr tree. Each "use" node's
// Params is resolved against r using the registered condition type's own
// parameter type, so the result round-trips through build(r) exactly like
// one constructed via Use/ExprAnd/ExprOr/ExprNot.
func UnmarshalConditionExpr(data []byte, r *ConditionRegistry) (ConditionExpr, error) {
	var node conditionExprJSON
	if err := json.Unmarshal(data, &node); err != nil {
		return nil, fmt.Errorf("policy: unmarshalling condition expression: %w", err)
	}
	return unmarshalConditionExprNode(node, r)
}

func unmarshalConditionExprNode(node conditionExprJSON, r *ConditionRegistry) (ConditionExpr, error) {
	switch node.Type {
	case "use":
		params, err := r.unmarshalParams(node.Name, node.Params)
		if err != nil {
			return nil, err
		}
		return &useExpr{name: node.Name, params: params}, nil
	case "and":
		exprs, err := unmarshalConditionExprNodes(node.Children, r)
		if err != nil {
			return nil, err
		}
		return &andExpr{exprs: exprs}, nil
	case "or":
		exprs, err := unmarshalConditionExprNodes(node.Children, r)
		if err != nil {
			return nil, err
		}
		return &orExpr{exprs: exprs}, nil
	case "not":
		if len(node.Children) != 1 {
			return nil, fmt.Errorf("policy: \"not\" condition expression must have exactly one child, got %d", len(node.Children))
		}
		child, err := unmarshalConditionExprNode(node.Children[0], r)
		if err != nil {
			return nil, err
		}
		return &notExpr{expr: child}, nil
	default:
		return nil, fmt.Errorf("policy: unknown condition expression type %q", node.Type)
	}
}

func unmarshalConditionExprNodes(nodes []conditionExprJSON, r *ConditionRegistry) ([]ConditionExpr, error) {
	exprs := make([]ConditionExpr, 0, len(nodes))
	for _, node := range nodes {
		expr, err := unmarshalConditionExprNode(node, r)
		if err != nil {
			return nil, err
		}
		exprs = append(exprs, expr)
	}
	return exprs, nil
}
