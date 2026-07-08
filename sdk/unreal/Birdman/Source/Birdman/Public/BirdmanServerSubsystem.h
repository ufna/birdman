// DRAFT — never compiled against a real engine yet (see plugin README.md).
// Thin UE wrapper over the frozen sdk/core contract (birdman/birdman.h,
// docs/specs/sdk.md §2).
#pragma once

#include "Containers/Ticker.h"
#include "CoreMinimal.h"
#include "Subsystems/GameInstanceSubsystem.h"

#include "BirdmanServerSubsystem.generated.h"

#if BIRDMAN_WITH_CORE
namespace birdman { class ServerLink; }
#endif

class AController;
class AGameModeBase;
class APlayerController;

UENUM(BlueprintType)
enum class EBirdmanMatchResult : uint8
{
	Completed,
	Aborted
};

/** Master allocated a match to this server. Fired on the game thread. */
DECLARE_DYNAMIC_MULTICAST_DELEGATE_ThreeParams(FBirdmanOnAllocated,
	const FString&, MatchId,
	int32, PlayersExpected,
	const TMap<FString, FString>&, Metadata);

/** Finish the current match within DeadlineSeconds, do not start a new one. */
DECLARE_DYNAMIC_MULTICAST_DELEGATE_TwoParams(FBirdmanOnDrain,
	float, DeadlineSeconds,
	const FString&, Reason);

/**
 * Server side of the birdman platform in a dedicated server.
 *
 * Managed mode is automatic: the agent injects BIRDMAN_SOCKET/BIRDMAN_SERVER_ID/
 * BIRDMAN_PORT into the container; without them (PIE, local runs) everything is
 * a safe no-op and IsManaged() == false — no ifdefs in game code.
 *
 * Game obligations (docs/specs/sdk.md §2):
 *  1. NotifyReady() when a match can be accepted (<=30s from process start).
 *  2. OnAllocated -> prepare the match; when players connect, NotifyMatchStart().
 *  3. NotifyMatchEnd() -> then the process must exit by itself
 *     (FGenericPlatformMisc::RequestExit) — dedicated servers are one-shot.
 *  4. OnDrainRequested -> no new rounds; finish within the deadline.
 *
 * Player count is auto-tracked via AGameModeBase PostLogin/Logout events;
 * calling SetPlayerCount() once switches to manual mode. tick_ms is reported
 * automatically every 5 seconds. Delegates fire on the game thread.
 */
UCLASS()
class BIRDMAN_API UBirdmanServerSubsystem : public UGameInstanceSubsystem
{
	GENERATED_BODY()

public:
	//~ USubsystem
	virtual void Initialize(FSubsystemCollectionBase& Collection) override;
	virtual void Deinitialize() override;

	/** True when running under a birdman agent (managed dedicated server). */
	UFUNCTION(BlueprintPure, Category = "Birdman")
	bool IsManaged() const;

	/** Server can accept a match: map loaded, ports listening. Required <=30s from start. */
	UFUNCTION(BlueprintCallable, Category = "Birdman")
	void NotifyReady();

	/** The allocated match actually began (players connected). */
	UFUNCTION(BlueprintCallable, Category = "Birdman")
	void NotifyMatchStart();

	/** Match over. After this the process must exit by itself (one-shot server). */
	UFUNCTION(BlueprintCallable, Category = "Birdman")
	void NotifyMatchEnd(EBirdmanMatchResult Result);

	/** Manual player count override; disables PostLogin/Logout auto-tracking. */
	UFUNCTION(BlueprintCallable, Category = "Birdman")
	void SetPlayerCount(int32 Count);

	/** Custom gauge metric (<=1/s per name, coalesced by the SDK). */
	UFUNCTION(BlueprintCallable, Category = "Birdman")
	void ReportMetric(FName Name, double Value);

	/** Last allocated match id ("" before OnAllocated). */
	UFUNCTION(BlueprintPure, Category = "Birdman")
	FString GetMatchId() const;

	UPROPERTY(BlueprintAssignable, Category = "Birdman")
	FBirdmanOnAllocated OnAllocated;

	UPROPERTY(BlueprintAssignable, Category = "Birdman")
	FBirdmanOnDrain OnDrainRequested;

private:
	void HandlePostLogin(AGameModeBase* GameMode, APlayerController* NewPlayer);
	void HandleLogout(AGameModeBase* GameMode, AController* Exiting);
	void PushAutoPlayerCount(AGameModeBase* GameMode, int32 Delta);
	bool TickMetric(float DeltaTime);

#if BIRDMAN_WITH_CORE
	// Raw pointer on purpose: keeps birdman::ServerLink an incomplete type
	// for UHT-generated code; owned exclusively by this subsystem.
	birdman::ServerLink* Link = nullptr;
#endif

	FDelegateHandle PostLoginHandle;
	FDelegateHandle LogoutHandle;
	FTSTicker::FDelegateHandle MetricTickerHandle;

	/** SetPlayerCount() was called at least once -> auto-tracking is off. */
	bool bManualPlayerCount = false;

	double FrameTimeAccumMs = 0.0;
	int64 FrameSamples = 0;
	double MetricWindowSeconds = 0.0;
};
