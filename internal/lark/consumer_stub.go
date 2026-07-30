//go:build !lark

package lark

import (
	"context"
	"errors"
)

const noFeishuSupport = "built without Feishu support: rebuild with -tags lark"

// Consumer is unavailable in builds that omit the lark tag.
type Consumer struct{}

// NewConsumer reports that this binary does not include the Feishu SDK.
func NewConsumer(appID, appSecret string, h ActionHandler) (*Consumer, error) {
	return nil, errors.New(noFeishuSupport)
}

// Run reports that this binary does not include the Feishu SDK.
func (*Consumer) Run(ctx context.Context) error {
	return errors.New(noFeishuSupport)
}
