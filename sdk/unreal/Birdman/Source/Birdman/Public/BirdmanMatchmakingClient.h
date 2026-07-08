// DRAFT HEADER, INTENTIONALLY UNIMPLEMENTED — client-side matchmaking lands
// in the game-integration iteration (docs/specs/sdk.md §3). Shape of the API
// is part of the frozen v0 contract; bodies are stubs so the module links.
//
// TODO(game-integration iteration), implementation plan:
//  - MeasureQos: GET {master}/v1/qos -> region UDP-echo endpoints; 5 packets
//    per region in parallel, median RTT ("merit QoS" — no IP-geo guessing).
//  - RequestMatch: POST /v1/matchmaking/tickets {regions+rtt, client_version
//    (auto from ProjectVersion)}, then long-poll ?wait=25s until match/fail;
//    network errors retried with backoff, the ticket is recreated (master
//    restarts are invisible to the player); `update_required` surfaces as
//    its own failure reason for a dedicated UI path.
//  - CancelMatch: DELETE the ticket.
//  - Module deps to add in Birdman.Build.cs: "HTTP", "Json".
//  - Connect on found: ClientTravel(Host:Port) + "?join_token=" in options
//    when token verify is enabled.
#pragma once

#include "CoreMinimal.h"
#include "Subsystems/GameInstanceSubsystem.h"

#include "BirdmanMatchmakingClient.generated.h"

USTRUCT(BlueprintType)
struct FBirdmanQosResult
{
	GENERATED_BODY()

	UPROPERTY(BlueprintReadOnly, Category = "Birdman")
	FString Region;

	UPROPERTY(BlueprintReadOnly, Category = "Birdman")
	float RttMs = 0.0f;
};

USTRUCT(BlueprintType)
struct FBirdmanMatchRequest
{
	GENERATED_BODY()

	/** Regions with measured RTTs (from MeasureQos). */
	UPROPERTY(BlueprintReadWrite, Category = "Birdman")
	TArray<FBirdmanQosResult> Qos;

	/** Empty = auto-filled from ProjectVersion. */
	UPROPERTY(BlueprintReadWrite, Category = "Birdman")
	FString ClientVersion;
};

UENUM(BlueprintType)
enum class EBirdmanMatchFailReason : uint8
{
	Timeout,
	UpdateRequired, // client too old for the active server version — show the update UI
	Cancelled,
	Error
};

DECLARE_DYNAMIC_DELEGATE_OneParam(FBirdmanOnQosComplete, const TArray<FBirdmanQosResult>&, Results);
DECLARE_DYNAMIC_DELEGATE_FourParams(FBirdmanOnMatchFound,
	const FString&, Host, int32, Port, const FString&, MatchId, const FString&, JoinToken);
DECLARE_DYNAMIC_DELEGATE_OneParam(FBirdmanOnMatchFailed, EBirdmanMatchFailReason, Reason);

/**
 * Client-side matchmaking against the birdman master (REST + long-poll).
 * NOT IMPLEMENTED YET — see the TODO block at the top of this header.
 */
UCLASS()
class BIRDMAN_API UBirdmanMatchmakingClient : public UGameInstanceSubsystem
{
	GENERATED_BODY()

public:
	/** Measures RTT to every region (merit QoS). TODO: unimplemented stub. */
	UFUNCTION(BlueprintCallable, Category = "Birdman|Matchmaking")
	void MeasureQos(FBirdmanOnQosComplete OnComplete)
	{
		// TODO(game-integration iteration)
		OnComplete.ExecuteIfBound(TArray<FBirdmanQosResult>());
	}

	/** Creates a ticket and long-polls until a match is found. TODO: unimplemented stub. */
	void RequestMatch(const FBirdmanMatchRequest& Request,
		FBirdmanOnMatchFound OnFound,
		FBirdmanOnMatchFailed OnFailed)
	{
		// TODO(game-integration iteration)
		(void)Request;
		(void)OnFound;
		OnFailed.ExecuteIfBound(EBirdmanMatchFailReason::Error);
	}

	/** Cancels the outstanding ticket, if any. TODO: unimplemented stub. */
	UFUNCTION(BlueprintCallable, Category = "Birdman|Matchmaking")
	void CancelMatch()
	{
		// TODO(game-integration iteration)
	}
};
