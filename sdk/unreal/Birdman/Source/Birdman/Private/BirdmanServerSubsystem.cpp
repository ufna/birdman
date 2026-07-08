// DRAFT — never compiled against a real engine yet (see plugin README.md).
#include "BirdmanServerSubsystem.h"

#include "Async/Async.h"
#include "GameFramework/GameModeBase.h"
#include "Misc/App.h"

#if BIRDMAN_WITH_CORE
#include "birdman/birdman.h"
#endif

DEFINE_LOG_CATEGORY_STATIC(LogBirdman, Log, All);

namespace
{
	constexpr double GMetricWindowSeconds = 5.0; // sdk.md §2: tick_ms every 5s
}

void UBirdmanServerSubsystem::Initialize(FSubsystemCollectionBase& Collection)
{
	Super::Initialize(Collection);

#if BIRDMAN_WITH_CORE
	// The SDK itself no-ops without BIRDMAN_SOCKET, but only dedicated
	// servers should even try: clients/PIE never talk to an agent.
	if (!IsRunningDedicatedServer())
	{
		return;
	}

	Link = new birdman::ServerLink();

	birdman::Config Cfg;
	// kDispatch + AsyncTask: events hop from the SDK I/O thread to the game
	// thread; no ticking required.
	Cfg.callback_mode = birdman::CallbackMode::kDispatch;
	TWeakObjectPtr<UBirdmanServerSubsystem> WeakThis(this);
	Cfg.on_allocated = [WeakThis](const birdman::AllocatedEvent& Ev)
	{
		TMap<FString, FString> Metadata;
		for (const auto& Pair : Ev.metadata)
		{
			Metadata.Add(UTF8_TO_TCHAR(Pair.first.c_str()), UTF8_TO_TCHAR(Pair.second.c_str()));
		}
		const FString MatchId = UTF8_TO_TCHAR(Ev.match_id.c_str());
		const int32 PlayersExpected = Ev.players_expected;
		AsyncTask(ENamedThreads::GameThread, [WeakThis, MatchId, PlayersExpected, Metadata]()
		{
			if (UBirdmanServerSubsystem* Self = WeakThis.Get())
			{
				UE_LOG(LogBirdman, Log, TEXT("allocated: match_id=%s players_expected=%d"), *MatchId, PlayersExpected);
				Self->OnAllocated.Broadcast(MatchId, PlayersExpected, Metadata);
			}
		});
	};
	Cfg.on_drain_requested = [WeakThis](const birdman::DrainEvent& Ev)
	{
		const float Deadline = static_cast<float>(Ev.deadline_seconds);
		const FString Reason = UTF8_TO_TCHAR(Ev.reason.c_str());
		AsyncTask(ENamedThreads::GameThread, [WeakThis, Deadline, Reason]()
		{
			if (UBirdmanServerSubsystem* Self = WeakThis.Get())
			{
				UE_LOG(LogBirdman, Log, TEXT("drain requested: deadline=%.0fs reason=%s"), Deadline, *Reason);
				Self->OnDrainRequested.Broadcast(Deadline, Reason);
			}
		});
	};

	const bool bManaged = Link->Init(Cfg);
	UE_LOG(LogBirdman, Log, TEXT("birdman SDK %hs: managed=%s server_id=%hs port=%d"),
		birdman::SdkVersion().c_str(), bManaged ? TEXT("yes") : TEXT("no"),
		Link->ServerId().c_str(), Link->Port());
	if (!bManaged)
	{
		return; // standalone dedicated run: keep the link as a no-op shell
	}

	// Auto player tracking: count changes on PostLogin/Logout.
	PostLoginHandle = FGameModeEvents::GameModePostLoginEvent.AddUObject(
		this, &UBirdmanServerSubsystem::HandlePostLogin);
	LogoutHandle = FGameModeEvents::GameModeLogoutEvent.AddUObject(
		this, &UBirdmanServerSubsystem::HandleLogout);

	// Automatic tick_ms metric: sample every frame, report every 5s.
	MetricTickerHandle = FTSTicker::GetCoreTicker().AddTicker(
		FTickerDelegate::CreateUObject(this, &UBirdmanServerSubsystem::TickMetric));
#endif // BIRDMAN_WITH_CORE
}

void UBirdmanServerSubsystem::Deinitialize()
{
#if BIRDMAN_WITH_CORE
	if (MetricTickerHandle.IsValid())
	{
		FTSTicker::GetCoreTicker().RemoveTicker(MetricTickerHandle);
		MetricTickerHandle.Reset();
	}
	FGameModeEvents::GameModePostLoginEvent.Remove(PostLoginHandle);
	FGameModeEvents::GameModeLogoutEvent.Remove(LogoutHandle);
	if (Link != nullptr)
	{
		Link->Shutdown();
		delete Link;
		Link = nullptr;
	}
#endif
	Super::Deinitialize();
}

bool UBirdmanServerSubsystem::IsManaged() const
{
#if BIRDMAN_WITH_CORE
	return Link != nullptr && Link->IsManaged();
#else
	return false;
#endif
}

void UBirdmanServerSubsystem::NotifyReady()
{
#if BIRDMAN_WITH_CORE
	if (Link != nullptr)
	{
		Link->NotifyReady();
	}
#endif
}

void UBirdmanServerSubsystem::NotifyMatchStart()
{
#if BIRDMAN_WITH_CORE
	if (Link != nullptr)
	{
		Link->NotifyMatchStart();
	}
#endif
}

void UBirdmanServerSubsystem::NotifyMatchEnd(EBirdmanMatchResult Result)
{
#if BIRDMAN_WITH_CORE
	if (Link != nullptr)
	{
		Link->NotifyMatchEnd(Result == EBirdmanMatchResult::Completed
			? birdman::MatchResult::kCompleted
			: birdman::MatchResult::kAborted);
	}
#endif
}

void UBirdmanServerSubsystem::SetPlayerCount(int32 Count)
{
	bManualPlayerCount = true;
#if BIRDMAN_WITH_CORE
	if (Link != nullptr)
	{
		Link->SetPlayerCount(Count);
	}
#endif
}

void UBirdmanServerSubsystem::ReportMetric(FName Name, double Value)
{
#if BIRDMAN_WITH_CORE
	if (Link != nullptr)
	{
		Link->ReportMetric(TCHAR_TO_UTF8(*Name.ToString()), Value);
	}
#endif
}

FString UBirdmanServerSubsystem::GetMatchId() const
{
#if BIRDMAN_WITH_CORE
	if (Link != nullptr)
	{
		return UTF8_TO_TCHAR(Link->MatchId().c_str());
	}
#endif
	return FString();
}

void UBirdmanServerSubsystem::HandlePostLogin(AGameModeBase* GameMode, APlayerController* /*NewPlayer*/)
{
	PushAutoPlayerCount(GameMode, 0);
}

void UBirdmanServerSubsystem::HandleLogout(AGameModeBase* GameMode, AController* /*Exiting*/)
{
	// Logout broadcasts before the controller is removed: it is still counted.
	PushAutoPlayerCount(GameMode, -1);
}

void UBirdmanServerSubsystem::PushAutoPlayerCount(AGameModeBase* GameMode, int32 Delta)
{
	if (bManualPlayerCount || GameMode == nullptr || GameMode->GetGameInstance() != GetGameInstance())
	{
		return;
	}
#if BIRDMAN_WITH_CORE
	if (Link != nullptr)
	{
		Link->SetPlayerCount(FMath::Max(0, GameMode->GetNumPlayers() + Delta));
	}
#endif
}

bool UBirdmanServerSubsystem::TickMetric(float DeltaTime)
{
	FrameTimeAccumMs += static_cast<double>(DeltaTime) * 1000.0;
	FrameSamples++;
	MetricWindowSeconds += DeltaTime;
	if (MetricWindowSeconds >= GMetricWindowSeconds && FrameSamples > 0)
	{
#if BIRDMAN_WITH_CORE
		if (Link != nullptr)
		{
			// Average server frame time over the window. TODO: p95 as a
			// second metric once the panel plots it (sdk.md §2).
			Link->ReportMetric("tick_ms", FrameTimeAccumMs / static_cast<double>(FrameSamples));
		}
#endif
		FrameTimeAccumMs = 0.0;
		FrameSamples = 0;
		MetricWindowSeconds = 0.0;
	}
	return true; // keep ticking
}
