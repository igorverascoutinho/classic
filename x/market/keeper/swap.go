package keeper

import (
	sdk "github.com/cosmos/cosmos-sdk/types"

	core "github.com/classic-terra/core/v2/types"
	"github.com/classic-terra/core/v2/x/market/types"

	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

// ApplySwapToPool updates each pool with offerCoin and askCoin taken from swap operation,
// OfferPool = OfferPool + offerAmt (Fills the swap pool with offerAmt)
// AskPool = AskPool - askAmt       (Uses askAmt from the swap pool)
func (k Keeper) ApplySwapToPool(ctx sdk.Context, offerCoin sdk.Coin, askCoin sdk.DecCoin) error {
	// No delta update in case Terra to Terra swap
	if offerCoin.Denom != core.MicroLunaDenom && askCoin.Denom != core.MicroLunaDenom {
		return nil
	}

	terraPoolDelta := k.GetTerraPoolDelta(ctx)

	// In case swapping Terra to Luna, the terra swap pool(offer) must be increased and the luna swap pool(ask) must be decreased
	if offerCoin.Denom != core.MicroLunaDenom && askCoin.Denom == core.MicroLunaDenom {
		offerBaseCoin, err := k.ComputeInternalSwap(ctx, sdk.NewDecCoinFromCoin(offerCoin), core.MicroSDRDenom, true)
		if err != nil {
			return err
		}

		terraPoolDelta = terraPoolDelta.Add(offerBaseCoin.Amount)
	}

	// In case swapping Luna to Terra, the luna swap pool(offer) must be increased and the terra swap pool(ask) must be decreased
	if offerCoin.Denom == core.MicroLunaDenom && askCoin.Denom != core.MicroLunaDenom {
		askBaseCoin, err := k.ComputeInternalSwap(ctx, askCoin, core.MicroSDRDenom, true)
		if err != nil {
			return err
		}

		terraPoolDelta = terraPoolDelta.Sub(askBaseCoin.Amount)
	}

	k.SetTerraPoolDelta(ctx, terraPoolDelta)

	return nil
}

// ComputeSwap returns the amount of asked coins should be returned for a given offerCoin at the effective
// exchange rate registered with the oracle.
// Returns an Error if the swap is recursive, or the coins to be traded are unknown by the oracle, or the amount
// to trade is too small.
func (k Keeper) ComputeSwap(ctx sdk.Context, offerCoin sdk.Coin, askDenom string) (retDecCoin sdk.DecCoin, spread sdk.Dec, err error) {
	// Return invalid recursive swap err
	if offerCoin.Denom == askDenom {
		return sdk.DecCoin{}, sdk.ZeroDec(), sdkerrors.Wrap(types.ErrRecursiveSwap, askDenom)
	}
	var okVSupply = (offerCoin.Denom == core.MicroLunaDenom)
	// Swap offer coin to base denom for simplicity of swap process
	baseOfferDecCoin, err := k.ComputeInternalSwap(ctx, sdk.NewDecCoinFromCoin(offerCoin), core.MicroSDRDenom, okVSupply)
	if err != nil {
		return sdk.DecCoin{}, sdk.Dec{}, err
	}

	// Get swap amount based on the oracle price
	retDecCoin, err = k.ComputeInternalSwap(ctx, baseOfferDecCoin, askDenom, false)
	if err != nil {
		return sdk.DecCoin{}, sdk.Dec{}, err
	}

	// Terra => Terra swap
	// Apply only tobin tax without constant product spread
	if offerCoin.Denom != core.MicroLunaDenom && askDenom != core.MicroLunaDenom {
		var tobinTax sdk.Dec
		offerTobinTax, err2 := k.OracleKeeper.GetTobinTax(ctx, offerCoin.Denom)
		if err2 != nil {
			return sdk.DecCoin{}, sdk.Dec{}, err2
		}

		askTobinTax, err2 := k.OracleKeeper.GetTobinTax(ctx, askDenom)
		if err2 != nil {
			return sdk.DecCoin{}, sdk.Dec{}, err2
		}

		// Apply highest tobin tax for the denoms in the swap operation
		if askTobinTax.GT(offerTobinTax) {
			tobinTax = askTobinTax
		} else {
			tobinTax = offerTobinTax
		}

		spread = tobinTax
		return retDecCoin, spread, nil
	}

	basePool := k.BasePool(ctx)
	minSpread := k.MinStabilitySpread(ctx)

	// constant-product, which by construction is square of base(equilibrium) pool
	cp := basePool.Mul(basePool)
	terraPoolDelta := k.GetTerraPoolDelta(ctx)
	terraPool := basePool.Add(terraPoolDelta)
	lunaPool := cp.Quo(terraPool)

	var offerPool sdk.Dec // base denom(usdr) unit
	var askPool sdk.Dec   // base denom(usdr) unit
	if offerCoin.Denom != core.MicroLunaDenom {
		// Terra->Luna swap
		offerPool = terraPool
		askPool = lunaPool
	} else {
		// Luna->Terra swap
		offerPool = lunaPool
		askPool = terraPool
	}

	// Get cp(constant-product) based swap amount
	// askBaseAmount = askPool - cp / (offerPool + offerBaseAmount)
	// askBaseAmount is base denom(usdr) unit
	askBaseAmount := askPool.Sub(cp.Quo(offerPool.Add(baseOfferDecCoin.Amount)))

	// Both baseOffer and baseAsk are usdr units, so spread can be calculated by
	// spread = (baseOfferAmt - baseAskAmt) / baseOfferAmt
	baseOfferAmount := baseOfferDecCoin.Amount
	spread = baseOfferAmount.Sub(askBaseAmount).Quo(baseOfferAmount)

	if spread.LT(minSpread) {
		spread = minSpread
	}

	var errSup = k.ValidateSupplyMaximum(ctx, sdk.NewDecCoin(retDecCoin.Denom, retDecCoin.Amount.TruncateInt()))

	if errSup != nil {
		return sdk.DecCoin{}, sdk.ZeroDec(), errSup
	}

	return retDecCoin, spread, nil
}

// ComputeInternalSwap returns the amount of asked DecCoin should be returned for a given offerCoin at the effective
// exchange rate registered with the oracle.
// Different from ComputeSwap, ComputeInternalSwap does not charge a spread as its use is system internal.
func (k Keeper) ComputeInternalSwap(ctx sdk.Context, offerCoin sdk.DecCoin, askDenom string, notCheckSupply bool) (sdk.DecCoin, error) {
	if offerCoin.Denom == askDenom {
		return offerCoin, nil
	}

	offerRate, err := k.OracleKeeper.GetLunaExchangeRate(ctx, offerCoin.Denom)
	if err != nil {
		return sdk.DecCoin{}, sdkerrors.Wrap(types.ErrNoEffectivePrice, offerCoin.Denom)
	}

	askRate, err := k.OracleKeeper.GetLunaExchangeRate(ctx, askDenom)
	if err != nil {
		return sdk.DecCoin{}, sdkerrors.Wrap(types.ErrNoEffectivePrice, askDenom)
	}

	retAmount := offerCoin.Amount.Mul(askRate).Quo(offerRate)
	if retAmount.LTE(sdk.ZeroDec()) {
		return sdk.DecCoin{}, sdkerrors.Wrap(sdkerrors.ErrInvalidCoins, offerCoin.String())
	}
	var ok = "nao"
	if notCheckSupply {
		ok = "sim"
	}
	if !notCheckSupply {
		var errSup = k.ValidateSupplyMaximum(ctx, sdk.NewDecCoin(askDenom, retAmount.TruncateInt()))
		if errSup != nil {
			return sdk.DecCoin{}, sdkerrors.Wrap(errSup, " --- de onde "+ok+"  - "+offerCoin.Denom)
		}
	}

	return sdk.NewDecCoinFromDec(askDenom, retAmount), nil
}

// simulateSwap interface for simulate swap
func (k Keeper) simulateSwap(ctx sdk.Context, offerCoin sdk.Coin, askDenom string) (sdk.Coin, error) {
	if askDenom == offerCoin.Denom {
		return sdk.Coin{}, sdkerrors.Wrap(types.ErrRecursiveSwap, askDenom)
	}

	if offerCoin.Amount.BigInt().BitLen() > 100 {
		return sdk.Coin{}, sdkerrors.Wrap(sdkerrors.ErrInvalidCoins, offerCoin.String())
	}

	swapCoin, spread, err := k.ComputeSwap(ctx, offerCoin, askDenom)
	if err != nil {
		return sdk.Coin{}, sdkerrors.Wrap(sdkerrors.ErrPanic, err.Error())
	}

	if spread.IsPositive() {
		swapFeeAmt := spread.Mul(swapCoin.Amount)
		if swapFeeAmt.IsPositive() {
			swapFee := sdk.NewDecCoinFromDec(swapCoin.Denom, swapFeeAmt)
			swapCoin = swapCoin.Sub(swapFee)
		}
	}

	retCoin, _ := swapCoin.TruncateDecimal()
	var errSup = k.ValidateSupplyMaximum(ctx, sdk.NewDecCoin(retCoin.Denom, retCoin.Amount))
	if errSup != nil {
		return sdk.Coin{}, errSup
	}
	return retCoin, nil
}
func (k Keeper) ValidateSupplyMaximum(ctx sdk.Context, coin sdk.DecCoin) error {

	var ok, amount = isExists(coin.Denom, k.GetMaxSupplyCoin(ctx))
	var totalSupply = k.BankKeeper.GetSupply(ctx, coin.Denom)
	if ok {
		if (totalSupply.Amount.Int64() + coin.Amount.TruncateInt().Int64()) > amount.Int64() {
			//var decoin = sdk.NewDecCoin(retDecCoin.Denom, amount)
			return sdkerrors.Wrap(types.ErrZeroSwapCoin, "The value to be minted exceeded the maximum supply value "+amount.String()+coin.Denom)
		}
	} else {
		return sdkerrors.Wrap(types.ErrZeroSwapCoin, "maximum supply not configured for currency "+coin.Denom)
	}
	return nil
}
func isExists(demom string, coins []sdk.Coin) (result bool, amount sdk.Int) {
	result = false

	for _, coin := range coins {
		if coin.Denom == demom {
			amount = coin.Amount
			result = true
			break
		}
	}

	return result, amount
}
