package gamma

import "context"

const defaultPageSize = 100

// fetchAllMarkets iterates pages until all results are collected.
func (c *Client) fetchAllMarkets(ctx context.Context, opts ListMarketsOpts) ([]GammaMarket, error) {
	opts.Limit = defaultPageSize
	opts.Offset = 0

	var all []GammaMarket
	for {
		page, err := c.listMarketsPage(ctx, opts)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < opts.Limit {
			break
		}
		opts.Offset += opts.Limit
	}
	return all, nil
}

// fetchAllEvents iterates pages until all results are collected.
func (c *Client) fetchAllEvents(ctx context.Context, opts ListEventsOpts) ([]GammaEvent, error) {
	opts.Limit = defaultPageSize
	opts.Offset = 0

	var all []GammaEvent
	for {
		page, err := c.listEventsPage(ctx, opts)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < opts.Limit {
			break
		}
		opts.Offset += opts.Limit
	}
	return all, nil
}
